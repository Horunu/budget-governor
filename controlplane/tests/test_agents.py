from __future__ import annotations

import pytest


@pytest.mark.asyncio
async def test_create_agent_requires_admin_scope(client, tenant_with_admin_key):
    tenant, admin_key = tenant_with_admin_key
    # Issue a viewer key and confirm it cannot create agents.
    viewer_key = (
        await client.post(
            f"/v1/tenants/{tenant['id']}/api-keys",
            json={"scopes": ["viewer"]},
            headers={"Authorization": f"Bearer {admin_key}"},
        )
    ).json()["api_key"]

    resp = await client.post(
        "/v1/agents",
        json={"name": "billing-bot"},
        headers={"Authorization": f"Bearer {viewer_key}"},
    )
    assert resp.status_code == 403


@pytest.mark.asyncio
async def test_create_agent_issues_scoped_key(client, tenant_with_admin_key):
    tenant, admin_key = tenant_with_admin_key
    resp = await client.post(
        "/v1/agents",
        json={"name": "billing-bot"},
        headers={"Authorization": f"Bearer {admin_key}"},
    )
    assert resp.status_code == 201
    body = resp.json()
    assert body["tenant_id"] == tenant["id"]
    assert body["name"] == "billing-bot"
    assert body["api_key"].startswith("bg_live_")


@pytest.mark.asyncio
async def test_create_agent_duplicate_name_conflicts(client, tenant_with_admin_key):
    _, admin_key = tenant_with_admin_key
    headers = {"Authorization": f"Bearer {admin_key}"}
    resp1 = await client.post("/v1/agents", json={"name": "dup-bot"}, headers=headers)
    assert resp1.status_code == 201
    resp2 = await client.post("/v1/agents", json={"name": "dup-bot"}, headers=headers)
    assert resp2.status_code == 409


@pytest.mark.asyncio
async def test_agent_scoped_key_authenticates(client, tenant_with_admin_key):
    tenant, admin_key = tenant_with_admin_key
    agent_key = (
        await client.post(
            "/v1/agents",
            json={"name": "reader-bot"},
            headers={"Authorization": f"Bearer {admin_key}"},
        )
    ).json()["api_key"]

    # An agent-scoped key can read its own tenant's spend (empty, but
    # authorized) even though it lacks admin/viewer.
    resp = await client.get(
        f"/v1/spend/{tenant['id']}",
        headers={"Authorization": f"Bearer {agent_key}"},
    )
    assert resp.status_code == 200
