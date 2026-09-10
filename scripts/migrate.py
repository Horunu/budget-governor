#!/usr/bin/env python3
"""Applies numbered SQL files under migrations/ to Postgres, in order.

Why a small custom runner instead of Alembic: the schema in migrations/ is
shared across the Go gateway and multiple Python services (see
docs/DECISIONS.md ADR-007) -- plain, ordered .sql files keep the schema
definition readable and applicable from any language/tool, without asking
whoever's touching the Go gateway to understand Alembic's revision graph.

Usage:
    python scripts/migrate.py                 # apply all pending migrations
    python scripts/migrate.py --dsn postgres://...
"""

from __future__ import annotations

import argparse
import pathlib
import sys

import psycopg

MIGRATIONS_DIR = pathlib.Path(__file__).resolve().parent.parent / "migrations"


def applied_versions(conn: psycopg.Connection) -> set[str]:
    with conn.cursor() as cur:
        cur.execute(
            "CREATE TABLE IF NOT EXISTS schema_migrations ("
            "  version TEXT PRIMARY KEY,"
            "  applied_at TIMESTAMPTZ NOT NULL DEFAULT now()"
            ")"
        )
        cur.execute("SELECT version FROM schema_migrations")
        return {row[0] for row in cur.fetchall()}


def pending_migrations(applied: set[str]) -> list[pathlib.Path]:
    files = sorted(MIGRATIONS_DIR.glob("*.sql"))
    return [f for f in files if f.name not in applied]


def apply_migration(conn: psycopg.Connection, path: pathlib.Path) -> None:
    sql = path.read_text()
    with conn.cursor() as cur:
        cur.execute(sql)
        cur.execute(
            "INSERT INTO schema_migrations (version) VALUES (%s)", (path.name,)
        )
    conn.commit()


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--dsn",
        default=None,
        help="Postgres DSN. Defaults to $POSTGRES_DSN.",
    )
    args = parser.parse_args()

    import os

    dsn = args.dsn or os.environ.get(
        "POSTGRES_DSN",
        "postgres://budget_governor:budget_governor@localhost:5432/budget_governor",
    )

    with psycopg.connect(dsn) as conn:
        applied = applied_versions(conn)
        pending = pending_migrations(applied)
        if not pending:
            print("no pending migrations")
            return 0
        for path in pending:
            print(f"applying {path.name} ...")
            apply_migration(conn, path)
            print(f"applied {path.name}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
