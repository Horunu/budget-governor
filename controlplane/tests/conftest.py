from __future__ import annotations

import os

import pytest
import pytest_asyncio
from httpx import ASGITransport, AsyncClient
from sqlalchemy.ext.asyncio import async_sessionmaker, create_async_engine

os.environ.setdefault("PLATFORM_BOOTSTRAP_TOKEN", "test-platform-token")

from app.db import database  # noqa: E402
from app.db.models import Base  # noqa: E402
from app.main import app  # noqa: E402


@pytest_asyncio.fixture
async def db_session_factory():
    """Fresh in-memory SQLite database per test, with the full schema
    created from the ORM models. See docs/DECISIONS.md ADR-007 and
    controlplane/app/db/models.py's module docstring for why SQLite here
    is a deliberate, documented test-only substitute for Postgres (this
    sandboxed build environment has no Docker daemon to run a real
    Postgres against; `make test-controlplane` in a normal dev/CI
    environment can be pointed at real Postgres via TEST_POSTGRES_DSN).
    """
    engine = create_async_engine("sqlite+aiosqlite:///:memory:")
    async with engine.begin() as conn:
        await conn.run_sync(Base.metadata.create_all)

    factory = async_sessionmaker(engine, expire_on_commit=False)

    async def override_get_session():
        async with factory() as session:
            yield session

    app.dependency_overrides[database.get_session] = override_get_session
    yield factory
    app.dependency_overrides.clear()
    await engine.dispose()


@pytest_asyncio.fixture
async def client(db_session_factory):
    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as ac:
        yield ac


@pytest.fixture
def platform_token() -> str:
    return os.environ["PLATFORM_BOOTSTRAP_TOKEN"]


@pytest_asyncio.fixture
async def tenant_with_admin_key(client, platform_token):
    """Creates a fresh tenant and returns (tenant_dict, raw_admin_key) --
    the common setup nearly every non-tenant-creation test needs.
    """
    headers = {"X-Platform-Token": platform_token}
    tenant = (await client.post("/v1/tenants", json={"name": "fixture-tenant"}, headers=headers)).json()
    admin_key = (
        await client.post(f"/v1/tenants/{tenant['id']}/api-keys", json={"scopes": ["admin"]}, headers=headers)
    ).json()["api_key"]
    return tenant, admin_key
