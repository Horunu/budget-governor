from __future__ import annotations

from datetime import datetime, timezone

import pytest

from app.db.models import ReconciliationDrift, ReconciliationRun


async def _seed_run_and_drift(db_session_factory, tenant_id: str, resolved: bool) -> None:
    async with db_session_factory() as session:
        run = ReconciliationRun(
            started_at=datetime.now(timezone.utc),
            completed_at=datetime.now(timezone.utc),
            window_start=datetime.now(timezone.utc),
            window_end=datetime.now(timezone.utc),
            drift_detected=True,
            drift_amount_usd=12.5,
        )
        session.add(run)
        await session.flush()

        drift = ReconciliationDrift(
            run_id=run.id,
            tenant_id=tenant_id,
            gateway_observed_usd=100.0,
            provider_reported_usd=87.5,
            drift_amount_usd=12.5,
            drift_pct=0.125,
            resolved_at=datetime.now(timezone.utc) if resolved else None,
        )
        session.add(drift)
        await session.commit()


@pytest.mark.asyncio
async def test_unresolved_incidents_listed_by_default(client, tenant_with_admin_key, db_session_factory):
    tenant, admin_key = tenant_with_admin_key
    await _seed_run_and_drift(db_session_factory, tenant["id"], resolved=False)
    await _seed_run_and_drift(db_session_factory, tenant["id"], resolved=True)

    resp = await client.get("/v1/incidents", headers={"Authorization": f"Bearer {admin_key}"})
    assert resp.status_code == 200
    body = resp.json()
    assert len(body) == 1
    assert body[0]["resolved_at"] is None
    assert abs(body[0]["drift_amount_usd"] - 12.5) < 1e-9


@pytest.mark.asyncio
async def test_incidents_include_resolved_when_requested(client, tenant_with_admin_key, db_session_factory):
    tenant, admin_key = tenant_with_admin_key
    await _seed_run_and_drift(db_session_factory, tenant["id"], resolved=False)
    await _seed_run_and_drift(db_session_factory, tenant["id"], resolved=True)

    resp = await client.get(
        "/v1/incidents?unresolved=false", headers={"Authorization": f"Bearer {admin_key}"}
    )
    assert resp.status_code == 200
    assert len(resp.json()) == 2


@pytest.mark.asyncio
async def test_incidents_scoped_to_own_tenant_only(client, tenant_with_admin_key, db_session_factory, platform_token):
    tenant, admin_key = tenant_with_admin_key
    await _seed_run_and_drift(db_session_factory, tenant["id"], resolved=False)

    other = (
        await client.post(
            "/v1/tenants", json={"name": "other-tenant"}, headers={"X-Platform-Token": platform_token}
        )
    ).json()
    await _seed_run_and_drift(db_session_factory, other["id"], resolved=False)

    resp = await client.get("/v1/incidents", headers={"Authorization": f"Bearer {admin_key}"})
    body = resp.json()
    assert len(body) == 1
    assert body[0]["tenant_id"] == tenant["id"]
