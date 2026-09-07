from __future__ import annotations

import pytest


@pytest.mark.asyncio
async def test_create_tenant_requires_platform_token(client):
    resp = await client.post("/v1/tenants", json={"name": "acme-corp"})
    assert resp.status_code == 401


@pytest.mark.asyncio
async def test_create_tenant_with_platform_token(client, platform_token):
    resp = await client.post(
        "/v1/tenants",
        json={"name": "acme-corp"},
        headers={"X-Platform-Token": platform_token},
    )
    assert resp.status_code == 201
    body = resp.json()
    assert body["name"] == "acme-corp"
    assert body["id"].startswith("tenant_")


@pytest.mark.asyncio
async def test_create_tenant_duplicate_name_conflicts(client, platform_token):
    headers = {"X-Platform-Token": platform_token}
    resp1 = await client.post("/v1/tenants", json={"name": "beta-startup"}, headers=headers)
    assert resp1.status_code == 201
    resp2 = await client.post("/v1/tenants", json={"name": "beta-startup"}, headers=headers)
    assert resp2.status_code == 409


@pytest.mark.asyncio
async def test_issue_api_key_with_platform_token(client, platform_token):
    headers = {"X-Platform-Token": platform_token}
    tenant = (await client.post("/v1/tenants", json={"name": "charity-org"}, headers=headers)).json()

    resp = await client.post(
        f"/v1/tenants/{tenant['id']}/api-keys",
        json={"scopes": ["admin"], "label": "root"},
        headers=headers,
    )
    assert resp.status_code == 201
    body = resp.json()
    assert body["tenant_id"] == tenant["id"]
    assert body["scopes"] == ["admin"]
    assert body["api_key"].startswith("bg_live_")


@pytest.mark.asyncio
async def test_issue_api_key_with_existing_admin_key_same_tenant(client, platform_token):
    headers = {"X-Platform-Token": platform_token}
    tenant = (await client.post("/v1/tenants", json={"name": "rotator-inc"}, headers=headers)).json()
    admin_key = (
        await client.post(
            f"/v1/tenants/{tenant['id']}/api-keys",
            json={"scopes": ["admin"]},
            headers=headers,
        )
    ).json()["api_key"]

    # A second key issued using the tenant's own admin key, no platform
    # token required.
    resp = await client.post(
        f"/v1/tenants/{tenant['id']}/api-keys",
        json={"scopes": ["viewer"]},
        headers={"Authorization": f"Bearer {admin_key}"},
    )
    assert resp.status_code == 201
    assert resp.json()["scopes"] == ["viewer"]


@pytest.mark.asyncio
async def test_issue_api_key_rejects_admin_key_from_other_tenant(client, platform_token):
    headers = {"X-Platform-Token": platform_token}
    tenant_a = (await client.post("/v1/tenants", json={"name": "tenant-a"}, headers=headers)).json()
    tenant_b = (await client.post("/v1/tenants", json={"name": "tenant-b"}, headers=headers)).json()
    admin_key_a = (
        await client.post(f"/v1/tenants/{tenant_a['id']}/api-keys", json={"scopes": ["admin"]}, headers=headers)
    ).json()["api_key"]

    resp = await client.post(
        f"/v1/tenants/{tenant_b['id']}/api-keys",
        json={"scopes": ["admin"]},
        headers={"Authorization": f"Bearer {admin_key_a}"},
    )
    assert resp.status_code == 401


@pytest.mark.asyncio
async def test_issue_api_key_rejects_invalid_scope(client, platform_token):
    headers = {"X-Platform-Token": platform_token}
    tenant = (await client.post("/v1/tenants", json={"name": "scope-test"}, headers=headers)).json()
    resp = await client.post(
        f"/v1/tenants/{tenant['id']}/api-keys",
        json={"scopes": ["superuser"]},
        headers=headers,
    )
    assert resp.status_code == 400
