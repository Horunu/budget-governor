from __future__ import annotations

import pytest
from httpx import ASGITransport, AsyncClient

from app.main import app


@pytest.mark.asyncio
async def test_health_endpoint():
    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        resp = await client.get("/v1/health")
    assert resp.status_code == 200
    assert resp.json()["status"] == "ok"


@pytest.mark.asyncio
async def test_advise_endpoint_returns_suggestions():
    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        resp = await client.post(
            "/v1/advise",
            json={
                "tenant_id": "tenant-acme",
                "agent_id": "agent-1",
                "requested_model": "claude-opus-5",
                "estimated_tokens": 500,
                "remaining_tokens": 0,
                "remaining_usd": 0,
                "failed_scope": "tenant",
                "recent_calls": [],
            },
        )
    assert resp.status_code == 200
    body = resp.json()
    assert "suggestions" in body


@pytest.mark.asyncio
async def test_advise_endpoint_rejects_malformed_request():
    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        resp = await client.post("/v1/advise", json={"tenant_id": "x"})  # missing required fields
    assert resp.status_code == 422
