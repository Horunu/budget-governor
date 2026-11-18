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


@pytest.mark.asyncio
async def test_list_rotate_and_remove_agent(client, tenant_with_admin_key):
    _, admin_key = tenant_with_admin_key
    admin = {"Authorization": f"Bearer {admin_key}"}
    created = (await client.post("/v1/agents", json={"name": "bob"}, headers=admin)).json()
    old_key = created["api_key"]

    listed = (await client.get("/v1/agents", headers=admin)).json()
    assert [(a["name"], a["active"]) for a in listed] == [("bob", True)]

    rotated = await client.post(f"/v1/agents/{created['id']}/rotate-key", headers=admin)
    assert rotated.status_code == 200
    new_key = rotated.json()["api_key"]
    assert new_key != old_key
    assert (await client.get("/v1/agents", headers={"Authorization": f"Bearer {old_key}"})).status_code == 401

    assert (await client.delete(f"/v1/agents/{created['id']}", headers=admin)).status_code == 204
    assert (await client.get("/v1/agents", headers={"Authorization": f"Bearer {new_key}"})).status_code == 401
    listed = (await client.get("/v1/agents", headers=admin)).json()
    assert listed[0]["active"] is False


@pytest.mark.asyncio
async def test_agent_endpoints_scoped_to_own_tenant(client, tenant_with_admin_key):
    _, admin_key = tenant_with_admin_key
    agent = (await client.post("/v1/agents", json={"name": "kyle"}, headers={"Authorization": f"Bearer {admin_key}"})).json()
    # The agent's own key lacks admin scope, so it cannot remove itself or list the team.
    member = {"Authorization": f"Bearer {agent['api_key']}"}
    assert (await client.delete(f"/v1/agents/{agent['id']}", headers=member)).status_code == 403
    assert (await client.get("/v1/agents", headers=member)).status_code == 403
    assert (await client.delete("/v1/agents/agent_does_not_exist", headers={"Authorization": f"Bearer {admin_key}"})).status_code == 404
