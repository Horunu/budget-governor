"""Control plane runtime configuration, loaded from environment variables."""

from __future__ import annotations

from urllib.parse import parse_qsl, urlencode, urlsplit, urlunsplit

from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    model_config = SettingsConfigDict(env_prefix="", extra="ignore")

    # Same POSTGRES_DSN env var the gateway and scripts/migrate.py use
    # (a plain postgres://... DSN, since those use lib/pq and psycopg
    # respectively) -- see async_postgres_dsn below for the
    # SQLAlchemy-async-driver-qualified form this service actually
    # connects with.
    postgres_dsn: str = (
        "postgresql://budget_governor:budget_governor@localhost:5432/budget_governor"
    )
    environment: str = "development"

    # Auth cache TTL mirrors the gateway's own -- both services independently
    # cache validated keys, but the control plane's admin-mutation endpoints
    # are already low-frequency, so this exists mainly for consistency.
    auth_cache_ttl_seconds: int = 60

    # Spend query defaults.
    default_spend_window: str = "24h"

    @property
    def async_postgres_dsn(self) -> str:
        """POSTGRES_DSN rewritten for SQLAlchemy's asyncpg driver, so every
        service can share one env var."""
        dsn = self.postgres_dsn
        for prefix in ("postgresql://", "postgres://"):
            if dsn.startswith(prefix):
                dsn = "postgresql+asyncpg://" + dsn[len(prefix):]
                break
        if not dsn.startswith("postgresql+asyncpg://"):
            return dsn

        # asyncpg rejects libpq's sslmode parameter, which the Go gateway needs.
        parsed = urlsplit(dsn)
        query = [(k, v) for k, v in parse_qsl(parsed.query) if k != "sslmode"]
        return urlunsplit(parsed._replace(query=urlencode(query)))


def get_settings() -> Settings:
    return Settings()
