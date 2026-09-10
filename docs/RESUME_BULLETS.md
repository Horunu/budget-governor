# Resume Bullets → Code

Each bullet below is backed by working, runnable code in this repo, not
prose. Run `make up && make seed && make smoke && make bench` to see all
five in action end-to-end.

---

### 1. "Designed and implemented a multi-tenant LLM-call gateway enforcing per-tenant, per-agent, and per-task token budgets with sub-10ms p99 overhead."

- Gateway entrypoint: [`gateway/cmd/gateway/main.go`](../gateway/cmd/gateway/main.go)
- Multi-tenant auth (API key → tenant + scopes, in-memory cache): [`gateway/internal/auth`](../gateway/internal/auth)
- Three-level budget hierarchy (tenant/agent/task) enforced in one atomic Redis round trip: [`gateway/internal/budget`](../gateway/internal/budget), Lua script at [`gateway/internal/budget/checkAndDecrement.lua`](../gateway/internal/budget/checkAndDecrement.lua)
- Reverse proxy / streaming logic that keeps the enforcement path off the response body: [`gateway/internal/proxy`](../gateway/internal/proxy)
- p99 overhead proof: [`loadtest/fanout_burst.js`](../loadtest/fanout_burst.js), run via `make bench` (see [`scripts/benchmark.sh`](../scripts/benchmark.sh)); methodology explained in [`docs/ARCHITECTURE.md#hot-path-latency-budget`](./ARCHITECTURE.md#hot-path-latency-budget)

### 2. "Built a distributed token-bucket enforcement layer validated under 5,000+ req/sec simulated agent fan-out load."

- Token bucket implementation: [`gateway/internal/budget/bucket.go`](../gateway/internal/budget/bucket.go), atomic Lua script [`gateway/internal/budget/checkAndDecrement.lua`](../gateway/internal/budget/checkAndDecrement.lua)
- Unit tests for refill, atomicity under concurrency, zero-budget edge cases: [`gateway/internal/budget/bucket_test.go`](../gateway/internal/budget/bucket_test.go)
- Distributed/multi-replica correctness argument: [`docs/ARCHITECTURE.md#budget-enforcement-hierarchy`](./ARCHITECTURE.md#budget-enforcement-hierarchy), decision rationale in [`docs/DECISIONS.md#adr-002`](./DECISIONS.md#adr-002-redis--lua-scripts-for-the-token-bucket-not-an-in-process-or-db-backed-counter)
- Load test demonstrating 5,000+ simulated concurrent agents fanning out across tenants: [`loadtest/fanout_burst.js`](../loadtest/fanout_burst.js), results captured by `make bench` into [`loadtest/results/`](../loadtest/results)

### 3. "Implemented a reconciliation pipeline comparing gateway-observed spend against provider billing APIs, detecting and correcting drift."

- Reconciliation job: [`reconciliation/job.py`](../reconciliation/job.py)
- Provider usage/cost API clients (OpenAI, Anthropic — real endpoint shapes): [`reconciliation/providers/`](../reconciliation/providers)
- Drift detection + threshold logic + incident creation: [`reconciliation/job.py`](../reconciliation/job.py) (`detect_drift`), schema in [`migrations/0001_init.sql`](../migrations/0001_init.sql) (`reconciliation_runs`, `reconciliation_drift`)
- Incidents surfaced via the control plane API: [`controlplane/app/api/incidents.py`](../controlplane/app/api/incidents.py) (`GET /v1/incidents`)
- Tests covering exact match, drift within threshold, drift exceeding threshold: [`reconciliation/tests/test_job.py`](../reconciliation/tests/test_job.py)

### 4. "Designed an LLM-as-advisor cost-optimization feature with a deterministic policy engine gating all decisions."

- Advisor service (LLM call + strict schema validation, never crashes on bad output): [`advisor/app/advisor.py`](../advisor/app/advisor.py), [`advisor/app/schemas.py`](../advisor/app/schemas.py)
- Deterministic policy engine (pure functions, same input → same output, the *only* component that turns a suggestion into a decision): [`gateway/internal/policy`](../gateway/internal/policy)
- Local deterministic guardrails on the advisor side (confidence gating before a suggestion is even returned to the gateway): [`advisor/app/policy.py`](../advisor/app/policy.py)
- Wiring: gateway calls advisor only on budget pressure — [`gateway/internal/proxy/advisor_client.go`](../gateway/internal/proxy/advisor_client.go)
- Decision rationale for keeping the LLM advisory-only: [`docs/DECISIONS.md#adr-003`](./DECISIONS.md#adr-003-the-cost-advisor-is-async-only-gated-by-a-deterministic-policy-engine-never-on-the-hot-path)
- Tests: schema validation failure → empty suggestions, valid suggestions, policy engine decision table: [`advisor/tests/test_advisor.py`](../advisor/tests/test_advisor.py), [`gateway/internal/policy/policy_test.go`](../gateway/internal/policy/policy_test.go)

### 5. "Shipped a full observability stack (structured logs, burn-rate metrics, pre-emptive budget alerts, Grafana dashboards) with alerts that fire before overspend, not after the bill."

- Structured JSON request logs (trace_id, tenant_id, agent_id, task_id, model, tokens, cost, decision): [`gateway/internal/observability/logging.go`](../gateway/internal/observability/logging.go)
- Prometheus metrics (latency histogram, tokens/cost counters, rejection counter by reason, bucket saturation gauge): [`gateway/internal/observability/metrics.go`](../gateway/internal/observability/metrics.go)
- Burn-rate / pre-emptive alert rules (fire on predicted exhaustion, not actual exhaustion): [`observability/prometheus/alerts.yml`](../observability/prometheus/alerts.yml) (`BurnRateHigh`)
- Grafana dashboards (Tenant Overview, Gateway Health, Reconciliation Drift), provisioned automatically: [`observability/grafana/dashboards/`](../observability/grafana/dashboards)
- Runbook entry per alert: [`docs/RUNBOOK.md`](./RUNBOOK.md)
