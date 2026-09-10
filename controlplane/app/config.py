"""Control plane runtime configuration, loaded from environment variables."""

from __future__ import annotations

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
        """The same DSN, qualified for SQLAlchemy's async driver.

        Kept as a derived property (rather than asking every deployer to
        set a second, asyncpg-specific env var) so .env.example only
        needs one POSTGRES_DSN that every service -- Go and Python alike
        -- can point at.
        """
        dsn = self.postgres_dsn
        if dsn.startswith("postgresql+asyncpg://"):
            return dsn
        if dsn.startswith("postgresql://"):
            return "postgresql+asyncpg://" + dsn[len("postgresql://") :]
        if dsn.startswith("postgres://"):
            return "postgresql+asyncpg://" + dsn[len("postgres://") :]
        return dsn


def get_settings() -> Settings:
    return Settings()
