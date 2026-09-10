from __future__ import annotations

from datetime import datetime, timedelta, timezone

import pytest

from app.db.models import SpendEvent


async def _insert_spend_event(db_session_factory, **kwargs) -> None:
    async with db_session_factory() as session:
        session.add(SpendEvent(**kwargs))
        await session.commit()


@pytest.mark.asyncio
async def test_spend_window_parsing_rejects_invalid_window(client, tenant_with_admin_key):
    tenant, admin_key = tenant_with_admin_key
    resp = await client.get(
        f"/v1/spend/{tenant['id']}?window=notawindow",
        headers={"Authorization": f"Bearer {admin_key}"},
    )
    assert resp.status_code == 400


@pytest.mark.asyncio
async def test_spend_aggregates_within_window(client, tenant_with_admin_key, db_session_factory):
    tenant, admin_key = tenant_with_admin_key
    now = datetime.now(timezone.utc)

    await _insert_spend_event(
        db_session_factory,
        tenant_id=tenant["id"], agent_id="agent-1", model="mock-small",
        tokens_in=100, tokens_out=50, cost_usd=0.01, trace_id="t1",
        decision="allowed", created_at=now - timedelta(minutes=5),
    )
    await _insert_spend_event(
        db_session_factory,
        tenant_id=tenant["id"], agent_id="agent-2", model="mock-large",
        tokens_in=200, tokens_out=100, cost_usd=0.5, trace_id="t2",
        decision="allowed", created_at=now - timedelta(minutes=10),
    )
    # Outside the 1h window -- must not be counted.
    await _insert_spend_event(
        db_session_factory,
        tenant_id=tenant["id"], agent_id="agent-1", model="mock-small",
        tokens_in=999, tokens_out=999, cost_usd=99.0, trace_id="t3",
        decision="allowed", created_at=now - timedelta(hours=3),
    )

    resp = await client.get(
        f"/v1/spend/{tenant['id']}?window=1h",
        headers={"Authorization": f"Bearer {admin_key}"},
    )
    assert resp.status_code == 200
    body = resp.json()
    assert body["total_requests"] == 2
    assert body["total_tokens_in"] == 300
    assert body["total_tokens_out"] == 150
    assert abs(body["total_cost_usd"] - 0.51) < 1e-9
    assert {m["model"] for m in body["by_model"]} == {"mock-small", "mock-large"}
    assert {a["agent_id"] for a in body["by_agent"]} == {"agent-1", "agent-2"}


@pytest.mark.asyncio
async def test_agent_scoped_key_only_sees_own_spend(client, tenant_with_admin_key, db_session_factory):
    tenant, admin_key = tenant_with_admin_key
    headers = {"Authorization": f"Bearer {admin_key}"}
    agent = (await client.post("/v1/agents", json={"name": "solo-bot"}, headers=headers)).json()
    agent_key = agent["api_key"]

    now = datetime.now(timezone.utc)
    await _insert_spend_event(
        db_session_factory,
        tenant_id=tenant["id"], agent_id=agent["id"], model="mock-small",
        tokens_in=10, tokens_out=5, cost_usd=0.001, trace_id="a1",
        decision="allowed", created_at=now,
    )
    await _insert_spend_event(
        db_session_factory,
        tenant_id=tenant["id"], agent_id="some-other-agent", model="mock-small",
        tokens_in=1000, tokens_out=500, cost_usd=1.0, trace_id="a2",
        decision="allowed", created_at=now,
    )

    resp = await client.get(
        f"/v1/spend/{tenant['id']}?window=24h",
        headers={"Authorization": f"Bearer {agent_key}"},
    )
    assert resp.status_code == 200
    body = resp.json()
    assert body["total_requests"] == 1
    assert body["total_tokens_in"] == 10

    # The admin key sees both.
    admin_resp = (
        await client.get(f"/v1/spend/{tenant['id']}?window=24h", headers={"Authorization": f"Bearer {admin_key}"})
    ).json()
    assert admin_resp["total_requests"] == 2
