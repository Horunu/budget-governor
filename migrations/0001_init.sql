-- 0001_init.sql
--
-- Shared schema for budget-governor. Applied in order by scripts/migrate.py
-- and read/written by three runtimes: the Go gateway (auth.Store reads
-- api_keys; budget.ConfigCache reads budgets; proxy.SpendWriter writes
-- spend_events), the Python control plane (owns writes to nearly every
-- table), and the Python reconciliation job (writes reconciliation_*).
-- See docs/DECISIONS.md ADR-007 for why this is plain SQL rather than
-- Alembic.

CREATE EXTENSION IF NOT EXISTS pgcrypto; -- gen_random_uuid()

CREATE TABLE tenants (
    id         TEXT PRIMARY KEY DEFAULT ('tenant_' || replace(gen_random_uuid()::text, '-', '')),
    name       TEXT NOT NULL UNIQUE,
    -- Maps this tenant to the upstream provider account/workspace the
    -- reconciliation job pulls billing data for (see reconciliation/job.py).
    openai_org_id      TEXT,
    anthropic_workspace_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE agents (
    id         TEXT PRIMARY KEY DEFAULT ('agent_' || replace(gen_random_uuid()::text, '-', '')),
    tenant_id  TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name)
);

-- API keys are stored bcrypt-hashed; key_prefix is the first 12 chars of
-- the raw key in plaintext, indexed, so the gateway's auth lookup narrows
-- to (usually) one row before the O(1) bcrypt verify instead of scanning
-- every active key. See docs/DECISIONS.md ADR-005.
-- scopes is stored as a JSON array (e.g. ["admin","agent"]) rather than a
-- native Postgres TEXT[] so the exact same column shape is portable to
-- SQLite in the control plane's test suite (see
-- controlplane/app/db/models.py) -- JSON is natively supported by both,
-- whereas TEXT[] is Postgres-only. The gateway's Go auth reader
-- (internal/auth) parses it with encoding/json accordingly.
-- agent_id is set only for scope=["agent"] keys minted by
-- POST /v1/agents (one key per agent, returned once at registration
-- time) -- it lets the control plane's spend-query endpoint enforce
-- "agent scope can read its own data only" without the gateway needing
-- it at all (the gateway instead takes agent_id from the client-supplied
-- X-Agent-Id header on each proxied call; see gateway/internal/auth,
-- which deliberately does not select this column).
CREATE TABLE api_keys (
    id         TEXT PRIMARY KEY DEFAULT ('key_' || replace(gen_random_uuid()::text, '-', '')),
    tenant_id  TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    agent_id   TEXT REFERENCES agents(id) ON DELETE CASCADE,
    key_prefix TEXT NOT NULL,
    hashed_key TEXT NOT NULL,
    scopes     JSONB NOT NULL,
    label      TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ
);
CREATE INDEX idx_api_keys_key_prefix ON api_keys(key_prefix) WHERE revoked_at IS NULL;
CREATE INDEX idx_api_keys_tenant_id ON api_keys(tenant_id);

-- Budget configuration. A row with agent_id NULL and task_id NULL is a
-- tenant-level budget; agent_id set + task_id NULL is agent-level;
-- both set is task-level. NULL token_limit/dollar_limit means that
-- dimension is not enforced for this row (see gateway/internal/budget).
CREATE TABLE budgets (
    id                   TEXT PRIMARY KEY DEFAULT ('budget_' || replace(gen_random_uuid()::text, '-', '')),
    tenant_id            TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    agent_id             TEXT REFERENCES agents(id) ON DELETE CASCADE,
    task_id              TEXT,
    token_limit          BIGINT,
    dollar_limit         NUMERIC(12, 6),
    refill_rate_per_min  NUMERIC(14, 4) NOT NULL DEFAULT 0,
    period_seconds       INTEGER NOT NULL DEFAULT 86400,
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, agent_id, task_id)
);
CREATE INDEX idx_budgets_tenant_id ON budgets(tenant_id);

-- Append-only spend ledger, written asynchronously (batched) by the
-- gateway's proxy.SpendWriter. This is the system of record the control
-- plane's spend-query endpoints aggregate over, and what the
-- reconciliation job compares against provider billing.
CREATE TABLE spend_events (
    id              BIGSERIAL PRIMARY KEY,
    tenant_id       TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    agent_id        TEXT REFERENCES agents(id) ON DELETE SET NULL,
    task_id         TEXT,
    model           TEXT NOT NULL,
    tokens_in       INTEGER NOT NULL DEFAULT 0,
    tokens_out      INTEGER NOT NULL DEFAULT 0,
    cost_usd        NUMERIC(14, 8) NOT NULL DEFAULT 0,
    trace_id        TEXT NOT NULL,
    decision        TEXT NOT NULL, -- allowed | rejected | advised_retry
    decision_reason TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Primary access pattern is "this tenant's spend in a time window" (the
-- control plane's GET /v1/spend/{tenant_id}?window=... endpoint and the
-- reconciliation job's per-tenant aggregation both filter this way).
CREATE INDEX idx_spend_events_tenant_created ON spend_events(tenant_id, created_at);
CREATE INDEX idx_spend_events_tenant_agent_created ON spend_events(tenant_id, agent_id, created_at);

-- One row per reconciliation job execution.
CREATE TABLE reconciliation_runs (
    id              BIGSERIAL PRIMARY KEY,
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at    TIMESTAMPTZ,
    window_start    TIMESTAMPTZ NOT NULL,
    window_end      TIMESTAMPTZ NOT NULL,
    drift_detected  BOOLEAN NOT NULL DEFAULT false,
    drift_amount_usd NUMERIC(14, 6) NOT NULL DEFAULT 0,
    error           TEXT
);

-- One row per tenant per run where drift exceeded the configured
-- threshold; this is what the control plane's
-- GET /v1/incidents?unresolved=true endpoint lists.
CREATE TABLE reconciliation_drift (
    id                BIGSERIAL PRIMARY KEY,
    run_id            BIGINT NOT NULL REFERENCES reconciliation_runs(id) ON DELETE CASCADE,
    tenant_id         TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    gateway_observed_usd  NUMERIC(14, 6) NOT NULL,
    provider_reported_usd NUMERIC(14, 6) NOT NULL,
    drift_amount_usd     NUMERIC(14, 6) NOT NULL,
    drift_pct             NUMERIC(7, 4) NOT NULL,
    resolved_at       TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_reconciliation_drift_unresolved ON reconciliation_drift(tenant_id) WHERE resolved_at IS NULL;
