from __future__ import annotations

import pytest


@pytest.mark.asyncio
async def test_upsert_budget_requires_admin(client, tenant_with_admin_key):
    tenant, admin_key = tenant_with_admin_key
    viewer_key = (
        await client.post(
            f"/v1/tenants/{tenant['id']}/api-keys",
            json={"scopes": ["viewer"]},
            headers={"Authorization": f"Bearer {admin_key}"},
        )
    ).json()["api_key"]

    resp = await client.post(
        f"/v1/budgets/{tenant['id']}",
        json={"token_limit": 100000},
        headers={"Authorization": f"Bearer {viewer_key}"},
    )
    assert resp.status_code == 403


@pytest.mark.asyncio
async def test_upsert_and_get_tenant_level_budget(client, tenant_with_admin_key):
    tenant, admin_key = tenant_with_admin_key
    headers = {"Authorization": f"Bearer {admin_key}"}

    resp = await client.post(
        f"/v1/budgets/{tenant['id']}",
        json={"token_limit": 1_000_000, "dollar_limit": 100.0, "refill_rate_per_min": 500},
        headers=headers,
    )
    assert resp.status_code == 200
    created = resp.json()
    assert created["agent_id"] is None
    assert created["task_id"] is None
    assert created["token_limit"] == 1_000_000
    assert created["dollar_limit"] == 100.0

    listed = (await client.get(f"/v1/budgets/{tenant['id']}", headers=headers)).json()
    assert len(listed) == 1
    assert listed[0]["id"] == created["id"]


@pytest.mark.asyncio
async def test_upsert_budget_is_idempotent_per_scope(client, tenant_with_admin_key):
    tenant, admin_key = tenant_with_admin_key
    headers = {"Authorization": f"Bearer {admin_key}"}

    first = (
        await client.post(f"/v1/budgets/{tenant['id']}", json={"token_limit": 1000}, headers=headers)
    ).json()
    second = (
        await client.post(f"/v1/budgets/{tenant['id']}", json={"token_limit": 5000}, headers=headers)
    ).json()

    # Same (tenant, agent=None, task=None) scope -> updates the same row,
    # doesn't create a second one.
    assert first["id"] == second["id"]
    assert second["token_limit"] == 5000

    listed = (await client.get(f"/v1/budgets/{tenant['id']}", headers=headers)).json()
    assert len(listed) == 1


@pytest.mark.asyncio
async def test_upsert_budget_requires_a_limit(client, tenant_with_admin_key):
    tenant, admin_key = tenant_with_admin_key
    resp = await client.post(
        f"/v1/budgets/{tenant['id']}",
        json={},
        headers={"Authorization": f"Bearer {admin_key}"},
    )
    assert resp.status_code == 400


@pytest.mark.asyncio
async def test_cannot_access_another_tenants_budgets(client, tenant_with_admin_key):
    tenant, admin_key = tenant_with_admin_key
    resp = await client.get(
        "/v1/budgets/some-other-tenant-id",
        headers={"Authorization": f"Bearer {admin_key}"},
    )
    assert resp.status_code == 403


@pytest.mark.asyncio
async def test_agent_and_task_scoped_budgets(client, tenant_with_admin_key):
    tenant, admin_key = tenant_with_admin_key
    headers = {"Authorization": f"Bearer {admin_key}"}
    agent = (
        await client.post("/v1/agents", json={"name": "scoped-bot"}, headers=headers)
    ).json()

    tenant_budget = (
        await client.post(f"/v1/budgets/{tenant['id']}", json={"token_limit": 1_000_000}, headers=headers)
    ).json()
    agent_budget = (
        await client.post(
            f"/v1/budgets/{tenant['id']}",
            json={"agent_id": agent["id"], "token_limit": 50_000},
            headers=headers,
        )
    ).json()
    task_budget = (
        await client.post(
            f"/v1/budgets/{tenant['id']}",
            json={"agent_id": agent["id"], "task_id": "task-1", "token_limit": 5_000},
            headers=headers,
        )
    ).json()

    assert len({tenant_budget["id"], agent_budget["id"], task_budget["id"]}) == 3

    listed = (await client.get(f"/v1/budgets/{tenant['id']}", headers=headers)).json()
    assert len(listed) == 3
