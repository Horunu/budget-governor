"""Async SQLAlchemy engine/session wiring."""

from __future__ import annotations

from collections.abc import AsyncGenerator

from sqlalchemy.ext.asyncio import AsyncSession, async_sessionmaker, create_async_engine

from app.config import get_settings


def make_engine(dsn: str | None = None):
    dsn = dsn or get_settings().async_postgres_dsn
    connect_args = {}
    if dsn.startswith("sqlite"):
        # SQLite needs this for use across the async test event loop /
        # FastAPI's threaded startup; irrelevant for Postgres.
        connect_args = {"check_same_thread": False}
    return create_async_engine(dsn, pool_pre_ping=True, connect_args=connect_args)


_engine = None
_session_factory: async_sessionmaker[AsyncSession] | None = None


def configure(dsn: str | None = None) -> None:
    """(Re)configures the module-level engine/session factory. Called
    once at app startup, and by tests to point at a temporary SQLite DB.
    """
    global _engine, _session_factory
    _engine = make_engine(dsn)
    _session_factory = async_sessionmaker(_engine, expire_on_commit=False)


async def get_session() -> AsyncGenerator[AsyncSession, None]:
    if _session_factory is None:
        configure()
    assert _session_factory is not None
    async with _session_factory() as session:
        yield session


async def dispose() -> None:
    if _engine is not None:
        await _engine.dispose()
