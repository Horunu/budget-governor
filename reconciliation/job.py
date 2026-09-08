"""Reconciliation job: compares gateway-observed spend (Postgres
spend_events, real-time) against what each provider itself reports as
billed, over a trailing CLOSED time window, and records drift that
exceeds a configurable threshold as an incident.

Run on a schedule (cron / Kubernetes CronJob / docker-compose's
reconciliation service with an internal APScheduler loop -- see
deploy/docker-compose.yml). See docs/DECISIONS.md ADR-004 for why this
is a separate scheduled job rather than gateway-embedded logic, and why
it reconciles a closed window rather than "right now": provider billing
APIs are eventually consistent and can lag real-time by hours.

Usage:
    python job.py                                   # reconciles "yesterday, UTC"
    python job.py --window-start 2026-09-01T00:00:00+00:00 --window-end 2026-09-02T00:00:00+00:00
"""

from __future__ import annotations

import argparse
import asyncio
import logging
import os
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
from typing import Awaitable, Callable

import asyncpg

from usage_providers.anthropic_usage import AnthropicUsageProvider
from usage_providers.mock_usage import MockUsageProvider
from usage_providers.openai_usage import OpenAIUsageProvider

logger = logging.getLogger("reconciliation")

DEFAULT_DRIFT_THRESHOLD_PCT = 0.05


# --- Pure, DB-independent core logic (directly unit tested) -----------------


@dataclass
class TenantDrift:
    tenant_id: str
    gateway_observed_usd: float
    provider_reported_usd: float
    drift_amount_usd: float
    drift_pct: float
    exceeds_threshold: bool


@dataclass
class ReconciliationResult:
    window_start: datetime
    window_end: datetime
    drift_detected: bool
    total_drift_amount_usd: float
    per_tenant: list[TenantDrift] = field(default_factory=list)
    error: str | None = None


def detect_drift(gateway_observed_usd: float, provider_reported_usd: float, threshold_pct: float) -> TenantDrift:
    """Pure function: computes drift and whether it exceeds threshold_pct.
    Handles the zero-provider-reported edge case (treat any nonzero
    gateway-observed spend against zero provider-reported spend as 100%
    drift, rather than dividing by zero) -- this is EXACTLY the "exceeds
    5%" default-drift-threshold in that situation since 100% > 5%.
    """
    drift_amount = gateway_observed_usd - provider_reported_usd
    if provider_reported_usd == 0:
        drift_pct = 1.0 if gateway_observed_usd != 0 else 0.0
    else:
        drift_pct = abs(drift_amount) / provider_reported_usd

    return TenantDrift(
        tenant_id="",  # filled in by the caller
        gateway_observed_usd=gateway_observed_usd,
        provider_reported_usd=provider_reported_usd,
        drift_amount_usd=drift_amount,
        drift_pct=drift_pct,
        exceeds_threshold=drift_pct > threshold_pct,
    )


async def reconcile_window(
    window_start: datetime,
    window_end: datetime,
    gateway_observed: dict[str, float],
    get_provider_reported: Callable[[str], Awaitable[float]],
    threshold_pct: float = DEFAULT_DRIFT_THRESHOLD_PCT,
) -> ReconciliationResult:
    """The core reconciliation algorithm, independent of Postgres or any
    specific provider client -- get_provider_reported is an injected
    async callable (tenant_id -> provider-reported USD) so this function
    is fully unit-testable with synthetic data (see tests/test_job.py)
    and reused identically by run_from_db below against real data.
    """
    per_tenant: list[TenantDrift] = []
    total_drift = 0.0
    any_exceeded = False

    for tenant_id, observed in gateway_observed.items():
        reported = await get_provider_reported(tenant_id)
        drift = detect_drift(observed, reported, threshold_pct)
        drift.tenant_id = tenant_id
        per_tenant.append(drift)
        if drift.exceeds_threshold:
            any_exceeded = True
            total_drift += abs(drift.drift_amount_usd)

    return ReconciliationResult(
        window_start=window_start,
        window_end=window_end,
        drift_detected=any_exceeded,
        total_drift_amount_usd=total_drift,
        per_tenant=per_tenant,
    )


# --- Postgres-backed wiring ---------------------------------------------------


async def fetch_gateway_observed(conn: asyncpg.Connection, window_start: datetime, window_end: datetime) -> dict[str, float]:
    rows = await conn.fetch(
        """
        SELECT tenant_id, COALESCE(SUM(cost_usd), 0) AS total
        FROM spend_events
        WHERE created_at >= $1 AND created_at < $2
        GROUP BY tenant_id
        """,
        window_start,
        window_end,
    )
    return {row["tenant_id"]: float(row["total"]) for row in rows}


async def fetch_tenant_account_refs(conn: asyncpg.Connection) -> dict[str, dict[str, str | None]]:
    rows = await conn.fetch("SELECT id, openai_org_id, anthropic_workspace_id FROM tenants")
    return {
        row["id"]: {"openai": row["openai_org_id"], "anthropic": row["anthropic_workspace_id"]}
        for row in rows
    }


def build_provider_lookup(
    tenant_refs: dict[str, dict[str, str | None]],
    gateway_observed: dict[str, float],
    window_start: datetime,
    window_end: datetime,
) -> Callable[[str], Awaitable[float]]:
    """Builds the get_provider_reported callable reconcile_window needs,
    picking a real provider client per tenant when admin keys AND a
    matching account ref are configured, falling back to
    MockUsageProvider (seeded from gateway_observed, see its module
    docstring) otherwise.
    """
    openai_admin_key = os.environ.get("OPENAI_ADMIN_KEY", "")
    anthropic_admin_key = os.environ.get("ANTHROPIC_ADMIN_KEY", "")

    openai_provider = OpenAIUsageProvider(openai_admin_key) if openai_admin_key else None
    anthropic_provider = AnthropicUsageProvider(anthropic_admin_key) if anthropic_admin_key else None

    # The tenant with the largest observed spend is the one MockUsageProvider
    # deliberately drifts past threshold, so the demo's incident is on the
    # tenant most visible in the dashboards.
    anomalous = max(gateway_observed, key=gateway_observed.get) if gateway_observed else None
    mock_provider = MockUsageProvider(reference_costs=gateway_observed, anomalous_account_ref=anomalous)

    async def lookup(tenant_id: str) -> float:
        refs = tenant_refs.get(tenant_id, {})
        if openai_provider and refs.get("openai"):
            return await openai_provider.get_cost_usd(refs["openai"], window_start, window_end)
        if anthropic_provider and refs.get("anthropic"):
            return await anthropic_provider.get_cost_usd(refs["anthropic"], window_start, window_end)
        return await mock_provider.get_cost_usd(tenant_id, window_start, window_end)

    return lookup


async def persist_result(conn: asyncpg.Connection, result: ReconciliationResult) -> int:
    run_id = await conn.fetchval(
        """
        INSERT INTO reconciliation_runs
            (started_at, completed_at, window_start, window_end, drift_detected, drift_amount_usd, error)
        VALUES (now(), now(), $1, $2, $3, $4, $5)
        RETURNING id
        """,
        result.window_start,
        result.window_end,
        result.drift_detected,
        result.total_drift_amount_usd,
        result.error,
    )

    for drift in result.per_tenant:
        if not drift.exceeds_threshold:
            continue
        await conn.execute(
            """
            INSERT INTO reconciliation_drift
                (run_id, tenant_id, gateway_observed_usd, provider_reported_usd, drift_amount_usd, drift_pct)
            VALUES ($1, $2, $3, $4, $5, $6)
            """,
            run_id,
            drift.tenant_id,
            drift.gateway_observed_usd,
            drift.provider_reported_usd,
            drift.drift_amount_usd,
            drift.drift_pct,
        )

    return run_id


async def run_from_db(dsn: str, window_start: datetime, window_end: datetime, threshold_pct: float) -> ReconciliationResult:
    conn = await asyncpg.connect(dsn)
    try:
        gateway_observed = await fetch_gateway_observed(conn, window_start, window_end)
        tenant_refs = await fetch_tenant_account_refs(conn)

        lookup = build_provider_lookup(tenant_refs, gateway_observed, window_start, window_end)
        result = await reconcile_window(window_start, window_end, gateway_observed, lookup, threshold_pct)
        await persist_result(conn, result)
        return result
    except Exception as exc:  # noqa: BLE001
        logger.exception("reconciliation run failed")
        await conn.execute(
            """
            INSERT INTO reconciliation_runs
                (started_at, completed_at, window_start, window_end, drift_detected, drift_amount_usd, error)
            VALUES (now(), NULL, $1, $2, false, 0, $3)
            """,
            window_start,
            window_end,
            str(exc),
        )
        raise
    finally:
        await conn.close()


def previous_utc_day() -> tuple[datetime, datetime]:
    now = datetime.now(timezone.utc)
    today_start = now.replace(hour=0, minute=0, second=0, microsecond=0)
    window_start = today_start - timedelta(days=1)
    window_end = today_start
    return window_start, window_end


def main() -> None:
    logging.basicConfig(level=logging.INFO)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--window-start", type=datetime.fromisoformat, default=None)
    parser.add_argument("--window-end", type=datetime.fromisoformat, default=None)
    parser.add_argument(
        "--threshold-pct",
        type=float,
        default=float(os.environ.get("RECONCILIATION_DRIFT_THRESHOLD_PCT", DEFAULT_DRIFT_THRESHOLD_PCT)),
    )
    args = parser.parse_args()

    if args.window_start and args.window_end:
        window_start, window_end = args.window_start, args.window_end
    else:
        window_start, window_end = previous_utc_day()

    dsn = os.environ.get(
        "POSTGRES_DSN", "postgres://budget_governor:budget_governor@localhost:5432/budget_governor"
    )

    result = asyncio.run(run_from_db(dsn, window_start, window_end, args.threshold_pct))
    logger.info(
        "reconciliation complete: window=%s..%s drift_detected=%s total_drift_usd=%.4f tenants=%d",
        window_start,
        window_end,
        result.drift_detected,
        result.total_drift_amount_usd,
        len(result.per_tenant),
    )


if __name__ == "__main__":
    main()
