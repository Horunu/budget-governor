# Build Summary

This is the closing status report for the Budget Governor build. It says
what exists, what's intentionally simplified (and why), and exactly what
to run to verify it yourself.

## What was built and where

| Phase | Component | Location |
|---|---|---|
| 0 | Architecture, decision log, resume-bullet map | `docs/ARCHITECTURE.md`, `docs/DECISIONS.md`, `docs/RESUME_BULLETS.md` |
| 1 | Gateway hot path (Go): auth, three-level Redis/Lua budget enforcement, OpenAI/Anthropic/Mock provider adapters, streaming proxy, policy engine, async spend writer, Prometheus metrics, structured logging | `gateway/` |
| 2 | Control plane (FastAPI): tenants, agents, API keys, budgets, spend queries, incidents | `controlplane/` |
| 3 | Cost advisor (FastAPI): LLM-backed suggestions, strict schema validation, local guardrails | `advisor/` |
| 4 | Reconciliation job: drift detection between gateway-observed and provider-billed spend | `reconciliation/` |
| 5 | Observability: Prometheus scrape configs + alert rules, 3 provisioned Grafana dashboards | `observability/` |
| 6 | Docker Compose stack, seed/smoke/benchmark scripts, k6 load tests | `deploy/`, `scripts/`, `loadtest/` |
| 7 | This document, finished README/RUNBOOK | `docs/`, `README.md` |

**Test coverage**: 102 tests across four independent suites, all passing
as of the last commit:

| Suite | Command | Count |
|---|---|---|
| Gateway (Go, incl. subtests) | `cd gateway && go test -v ./...` | 44 |
| Control plane (Python) | `cd controlplane && pytest -q` | 23 |
| Advisor (Python) | `cd advisor && pytest -q` | 20 |
| Reconciliation (Python) | `cd reconciliation && pytest -q` | 15 |

(Gateway count includes the concurrency race test in
`gateway/internal/budget/bucket_test.go` asserting exact admission
counts under 50 concurrent goroutines against a shared Redis bucket.)

## What was stubbed or simplified, and why

**This build environment has no Docker daemon** (the `docker` CLI is
present; `docker` commands fail with "cannot connect to the Docker
daemon"). This is the root cause of everything else in this section:

- **`docker compose up` / `make smoke` / `make bench` have not been run
  against a live stack in this environment.** The compose file is
  verified with `docker compose config` (syntax/interpolation/build-
  context correctness) and every Dockerfile's actual runtime behavior
  was cross-checked by reproducing its exact `COPY`/`WORKDIR` layout in
  a local temp directory and running the resulting entrypoint against
  the corresponding Python venv — this is how a real bug was caught and
  fixed (the bare `uvicorn` console-script doesn't add the working
  directory to `sys.path`, which broke `advisor`'s `import providers`;
  see the Sept 2026 commit fixing both Dockerfiles to use
  `python -m uvicorn`). But a full end-to-end run on real Docker,
  including the exact `make bench` p99 numbers, has not happened here —
  see "exact commands to verify" below for what to run and what to
  expect.
- **Control-plane tests run against SQLite, not Postgres** (see
  `docs/DECISIONS.md` ADR-009). The schema is kept portable
  (`JSONB`→`JSON`, `BIGSERIAL`→`with_variant`) specifically so this
  works; the real, deployed schema (`migrations/0001_init.sql`) is
  Postgres-only and is what `docker compose up` actually runs against.
- **Reconciliation's real provider clients (`OpenAIUsageProvider`,
  `AnthropicUsageProvider`) are implemented against verified current API
  shapes but have never made a live call** — no admin API keys were
  available to this build. The default path (`MockUsageProvider`) is
  what's actually exercised by tests and is what runs in `docker compose
  up` unless `OPENAI_ADMIN_KEY`/`ANTHROPIC_ADMIN_KEY` are set.
- **The gateway's and advisor's real OpenAI/Anthropic adapters are
  tested against recorded HTTP fixtures** (`gateway/internal/provider/
  testdata/`), not live APIs, for the same reason. The request/response
  shapes were researched live (web search against current provider
  docs) and are current as of the verification date in
  `gateway/internal/provider/pricing.go`, but "current as of a
  documentation lookup" and "verified against a live call" are
  different claims — treat the pricing table especially as something to
  re-verify before relying on it for real budget decisions.

Independent of the no-Docker constraint, a few things were deliberately
scoped down or deferred:

- **No Loki/Promtail.** Structured JSON to stdout + `docker compose
  logs` covers this build's needs; see the dedicated note in
  `docs/DECISIONS.md`.
- **No Alertmanager deployed.** `observability/prometheus/alerts.yml`
  is Alertmanager-ready (just needs a target added to
  `prometheus.yml`'s `alerting:` block) but no Alertmanager container is
  in `docker-compose.yml` — alerts are visible in Prometheus's own
  Alerts UI, not routed to Slack/PagerDuty/etc.
- **No incident-resolution endpoint.** `reconciliation_drift.resolved_at`
  exists in the schema and the incidents API can list unresolved
  incidents, but there's no `PATCH /v1/incidents/{id}/resolve` yet —
  noted in `docs/RUNBOOK.md`'s `ReconciliationDriftHigh` entry.
- **No automated cross-provider failover.** The `ProviderErrorSpike`
  alert (see `docs/RUNBOOK.md`) documents this as a manual response
  today; the policy engine doesn't currently have a "route this tenant's
  traffic to the other provider" action.
- **`spend_events` isn't partitioned.** Noted as a scaling follow-up in
  `docs/ARCHITECTURE.md`; fine at demo scale, worth revisiting before
  real production volume.
- **No CI pipeline.** Nothing runs the four test suites automatically on
  push — a natural next step (GitHub Actions matrix over
  `go test`/`pytest`, plus `gofmt`/`ruff`/`black` lint gates) not
  included here since it wasn't asked for.
- **The reconciliation job's OpenAI tenant mapping treats
  `tenants.openai_org_id` as a project ID** (OpenAI's Costs API scopes
  by project, not by a nested sub-org) — see the code comment in
  `reconciliation/usage_providers/openai_usage.py` for the full
  rationale; a real multi-project deployment would want a column named
  accordingly.

**No TODO comments were left in shipped code.** Every simplification
above is either fully isolated behind an interface (swap
`MockUsageProvider` for a real one via env vars, no code change) or is a
documented, bounded gap with a clear "what to build next," not a
half-finished code path.

## From demo-ready to production-ready

1. **Get real provider keys and re-verify pricing.** Set
   `OPENAI_API_KEY` / `ANTHROPIC_API_KEY` (inference) and
   `OPENAI_ADMIN_KEY` / `ANTHROPIC_ADMIN_KEY` (reconciliation) in `.env`.
   Before relying on this for real budget enforcement, re-verify
   `gateway/internal/provider/pricing.go` and `providers/pricing.py`
   against each provider's current pricing page — they're dated, not
   live-fetched, by design (see the ADR-008 discussion in
   `docs/DECISIONS.md`).
2. **Actually run the stack on a machine with Docker.** `make up &&
   make seed && make smoke && make bench` — this build environment
   could not do this step itself (no daemon); it's the single most
   important thing to do before trusting this beyond a portfolio demo.
3. **Security hardening**:
   - Rotate `PLATFORM_BOOTSTRAP_TOKEN` out of `.env.example`'s default
     and into a real secrets manager.
   - Put TLS in front of every service (currently plain HTTP inside the
     compose network — fine for a single-host demo, not for a real
     multi-host deployment).
   - Move secrets (API keys, DB credentials, the bootstrap token) out of
     environment variables and into a proper secrets manager for any
     non-local deployment.
   - Add rate limiting to the control plane's own auth endpoints
     (currently only the gateway's Redis-backed budget system is rate-
     limited; a brute-force attempt against `POST /v1/tenants/{id}/api-
     keys` isn't itself throttled).
4. **Observability follow-through**: deploy Alertmanager and wire
   `alerts.yml`'s rules to real notification channels; consider adding
   distributed tracing (the `trace_id` field is already threaded through
   every log line and would slot into OpenTelemetry/Jaeger cleanly).
5. **Scale follow-through**: partition `spend_events`; if Redis becomes
   the bottleneck at very high fan-out, shard it (Redis Cluster, keyed by
   tenant prefix — see `docs/ARCHITECTURE.md#scaling-considerations`).
6. **Add a CI pipeline** running all four test suites plus lint on every
   push, so the "128 tests passing" claim stays continuously true rather
   than being a point-in-time snapshot from this build.

## Exact commands to verify everything works

```bash
git clone <this-repo>
cd budget-governor
cp .env.example .env

make up          # docker compose up -d --build
make seed        # creates 3 demo tenants/agents/budgets, prints API keys
make smoke       # end-to-end: allowed calls, budget exhaustion, advisor,
                  # reconciliation, all asserted with a pass/fail summary
make bench       # k6 fan-out burst at 5,000+ req/s, reports p50/p95/p99
                  # on allowed requests + rejection rate + cost attribution

# Open Grafana at http://localhost:3000 (admin/admin) -- three
# dashboards under "Budget Governor" should already be provisioned and
# showing live data from the above.

# Per-service test suites (each has its own venv):
cd gateway && go test ./...

cd controlplane && python3 -m venv .venv && source .venv/bin/activate \
  && pip install -e ".[dev]" && PYTHONPATH=. pytest -q

cd advisor && python3 -m venv .venv && source .venv/bin/activate \
  && pip install -e ".[dev]" && pytest -q

cd reconciliation && python3 -m venv .venv && source .venv/bin/activate \
  && pip install -e ".[dev]" && pytest -q
```

All four suites (102 tests total) passed in this build's own sandboxed
environment (Go 1.24.7 / 1.25 toolchain, Python 3.11) prior to every
commit in the git history. What has NOT been executed in this
environment, for the Docker-daemon reason above, is `make up` through
`make bench` against a live stack — that is the one remaining
verification step for whoever runs this next.
