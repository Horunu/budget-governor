from __future__ import annotations

from datetime import datetime, timedelta, timezone

import pytest

from job import DEFAULT_DRIFT_THRESHOLD_PCT, detect_drift, reconcile_window
from usage_providers.mock_usage import MockUsageProvider


WINDOW_START = datetime(2026, 9, 1, tzinfo=timezone.utc)
WINDOW_END = WINDOW_START + timedelta(days=1)


# --- detect_drift: pure function, exact match / within / exceeding ----------


def test_detect_drift_exact_match():
    d = detect_drift(100.0, 100.0, DEFAULT_DRIFT_THRESHOLD_PCT)
    assert d.drift_amount_usd == 0.0
    assert d.drift_pct == 0.0
    assert d.exceeds_threshold is False


def test_detect_drift_within_threshold():
    # 3% drift, threshold is 5% -> should NOT exceed.
    d = detect_drift(103.0, 100.0, 0.05)
    assert abs(d.drift_pct - 0.03) < 1e-9
    assert d.exceeds_threshold is False


def test_detect_drift_exceeds_threshold():
    # 12% drift, threshold is 5% -> SHOULD exceed.
    d = detect_drift(112.0, 100.0, 0.05)
    assert abs(d.drift_pct - 0.12) < 1e-9
    assert d.exceeds_threshold is True


def test_detect_drift_exactly_at_threshold_does_not_exceed():
    # Strictly greater-than semantics: exactly at threshold is NOT
    # "exceeding" it.
    d = detect_drift(105.0, 100.0, 0.05)
    assert d.exceeds_threshold is False


def test_detect_drift_provider_reported_zero_nonzero_gateway_is_100pct_drift():
    d = detect_drift(50.0, 0.0, 0.05)
    assert d.drift_pct == 1.0
    assert d.exceeds_threshold is True


def test_detect_drift_both_zero_is_no_drift():
    d = detect_drift(0.0, 0.0, 0.05)
    assert d.drift_pct == 0.0
    assert d.exceeds_threshold is False


def test_detect_drift_gateway_under_reports_is_still_drift():
    # Gateway observed LESS than the provider billed -- equally real
    # drift (estimation error can go either direction), should still be
    # flagged based on magnitude.
    d = detect_drift(80.0, 100.0, 0.05)
    assert abs(d.drift_pct - 0.20) < 1e-9
    assert d.exceeds_threshold is True


# --- reconcile_window: multi-tenant orchestration ----------------------------


@pytest.mark.asyncio
async def test_reconcile_window_no_drift_detected_when_all_within_threshold():
    gateway_observed = {"tenant-a": 100.0, "tenant-b": 50.0}

    async def lookup(tenant_id: str) -> float:
        return {"tenant-a": 101.0, "tenant-b": 50.5}[tenant_id]  # ~1% drift each

    result = await reconcile_window(WINDOW_START, WINDOW_END, gateway_observed, lookup, threshold_pct=0.05)
    assert result.drift_detected is False
    assert result.total_drift_amount_usd == 0.0
    assert len(result.per_tenant) == 2
    assert all(not d.exceeds_threshold for d in result.per_tenant)


@pytest.mark.asyncio
async def test_reconcile_window_flags_only_the_tenant_that_exceeds():
    gateway_observed = {"tenant-a": 100.0, "tenant-b": 100.0}

    async def lookup(tenant_id: str) -> float:
        return {"tenant-a": 99.0, "tenant-b": 70.0}[tenant_id]  # a: 1%, b: 30%

    result = await reconcile_window(WINDOW_START, WINDOW_END, gateway_observed, lookup, threshold_pct=0.05)
    assert result.drift_detected is True

    flagged = [d for d in result.per_tenant if d.exceeds_threshold]
    assert len(flagged) == 1
    assert flagged[0].tenant_id == "tenant-b"
    assert abs(result.total_drift_amount_usd - 30.0) < 1e-9


@pytest.mark.asyncio
async def test_reconcile_window_empty_input_produces_empty_result():
    async def lookup(tenant_id: str) -> float:
        raise AssertionError("should not be called for an empty gateway_observed dict")

    result = await reconcile_window(WINDOW_START, WINDOW_END, {}, lookup, threshold_pct=0.05)
    assert result.drift_detected is False
    assert result.per_tenant == []
    assert result.total_drift_amount_usd == 0.0


# --- MockUsageProvider: realistic, bounded, deterministic perturbation ------


@pytest.mark.asyncio
async def test_mock_usage_provider_is_deterministic():
    provider = MockUsageProvider(reference_costs={"tenant-a": 200.0})
    v1 = await provider.get_cost_usd("tenant-a", WINDOW_START, WINDOW_END)
    v2 = await provider.get_cost_usd("tenant-a", WINDOW_START, WINDOW_END)
    assert v1 == v2


@pytest.mark.asyncio
async def test_mock_usage_provider_non_anomalous_stays_within_default_threshold():
    provider = MockUsageProvider(reference_costs={"tenant-a": 200.0}, anomalous_account_ref="tenant-other")
    reported = await provider.get_cost_usd("tenant-a", WINDOW_START, WINDOW_END)
    drift = detect_drift(200.0, reported, DEFAULT_DRIFT_THRESHOLD_PCT)
    assert drift.exceeds_threshold is False


@pytest.mark.asyncio
async def test_mock_usage_provider_anomalous_account_exceeds_default_threshold():
    provider = MockUsageProvider(reference_costs={"tenant-a": 200.0}, anomalous_account_ref="tenant-a")
    reported = await provider.get_cost_usd("tenant-a", WINDOW_START, WINDOW_END)
    drift = detect_drift(200.0, reported, DEFAULT_DRIFT_THRESHOLD_PCT)
    assert drift.exceeds_threshold is True


@pytest.mark.asyncio
async def test_mock_usage_provider_unknown_account_returns_zero():
    provider = MockUsageProvider(reference_costs={"tenant-a": 200.0})
    reported = await provider.get_cost_usd("unknown-tenant", WINDOW_START, WINDOW_END)
    assert reported == 0.0


@pytest.mark.asyncio
async def test_full_pipeline_end_to_end_via_mock_provider():
    """Exercises reconcile_window against MockUsageProvider directly --
    the same composition job.py's build_provider_lookup wires up for the
    no-API-keys default path.
    """
    gateway_observed = {"acme-corp": 500.0, "beta-startup": 20.0, "charity-org": 3.0}
    provider = MockUsageProvider(reference_costs=gateway_observed, anomalous_account_ref="acme-corp")

    async def lookup(tenant_id: str) -> float:
        return await provider.get_cost_usd(tenant_id, WINDOW_START, WINDOW_END)

    result = await reconcile_window(WINDOW_START, WINDOW_END, gateway_observed, lookup, threshold_pct=0.05)

    assert result.drift_detected is True
    flagged_ids = {d.tenant_id for d in result.per_tenant if d.exceeds_threshold}
    assert flagged_ids == {"acme-corp"}
