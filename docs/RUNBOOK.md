# Runbook

Operational guidance for running Budget Governor and responding to its
alerts. See [`docs/ARCHITECTURE.md`](./ARCHITECTURE.md) for system design
and [`docs/DECISIONS.md`](./DECISIONS.md) for why things are built this
way.

## Standard operations

| Task | Command |
|---|---|
| Bring up the whole stack | `make up` (`docker compose -f deploy/docker-compose.yml up -d --build`) |
| Apply database migrations | `make migrate` |
| Seed demo tenants/agents/budgets | `make seed` |
| Run the end-to-end smoke test | `make smoke` |
| Run the load test | `make bench` |
| Tear down | `make down` |
| View gateway logs (structured JSON) | `docker compose -f deploy/docker-compose.yml logs -f gateway \| jq .` |
| Open Grafana | http://localhost:3000 (default admin/admin, provisioned dashboards under "Budget Governor") |
| Open Prometheus | http://localhost:9091 |
| Query gateway metrics directly | `curl http://localhost:9090/metrics` |

## Alert runbook entries

### BurnRateHigh

**Fires when**: `predict_linear(bucket_saturation_ratio[15m], 3600) > 1`
for 5 minutes — the current spend trend for a tenant/agent/task bucket
projects budget exhaustion within the next hour. This is deliberately
*pre-emptive*: it fires before the budget is actually exhausted, based on
trend, not after.

**Likely cause**: a legitimate traffic spike, a runaway/looping agent, or
a budget ceiling that's simply too low for current usage.

**Response**:
1. Check the Tenant Overview dashboard for which tenant/scope is
   trending toward exhaustion, and the "top agents by cost" panel to see
   if one agent is driving it.
2. If it's a runaway agent (e.g. a tool-use loop re-calling the LLM
   repeatedly), that agent's operator should be notified — the token
   bucket will still hard-enforce the ceiling once reached, so this is
   not an emergency, but it is worth investigating before the tenant
   starts seeing 429s.
3. If it's legitimate, expected demand, raise the tenant's budget via
   `POST /v1/budgets/{tenant_id}` (admin scope) ahead of exhaustion
   rather than after.
4. If it's unexpected and looks like abuse or a bug, consider revoking
   the offending agent's API key (issue a fresh one after investigating)
   rather than waiting for the hard budget wall.

### ReconciliationDriftHigh

**Fires when**: the most recent unresolved reconciliation drift for a
tenant exceeds 10% (`pg_reconciliation_drift_drift_pct > 0.10`).

**Likely cause**: (a) the gateway's pre-flight token estimate
(`internal/provider.EstimateTokens`) has drifted from reality for this
tenant's typical prompt/response shape (e.g. heavy non-English or code
content, which the heuristic under-counts more than English prose), (b)
a provider pricing change that hasn't been reflected in
`gateway/internal/provider/pricing.go` / `providers/pricing.py`, or (c) a
genuine gap — mis-attributed spend, a billing-side outage, or (in a real
deployment) an account/workspace mapping error in `tenants.openai_org_id`
/ `tenants.anthropic_workspace_id`.

**Response**:
1. Check `GET /v1/incidents?unresolved=true` (or the Reconciliation
   Drift dashboard) for the exact drift amount and direction
   (gateway-observed higher or lower than provider-reported).
2. Re-verify the pricing table is current — see the "last verified" date
   in `gateway/internal/provider/pricing.go`; if a provider changed
   pricing since then, update both the Go and Python copies.
3. If the tenant's traffic is unusually code-heavy or non-English,
   consider this an expected estimation-accuracy gap (see
   `docs/DECISIONS.md` ADR-008) rather than a bug — the actual amount
   billed always comes from the provider's own response, so this doesn't
   indicate an overspend was missed, only that the *estimate* used for
   pre-flight budget checks was imprecise for this tenant's traffic
   shape.
4. Once understood, resolve the incident (`resolved_at` is set via a
   direct update today; there is no resolve endpoint yet).

### GatewayLatencyHigh

**Fires when**: p99 latency on *allowed* requests exceeds 10ms for 5
minutes (`histogram_quantile(0.99, ...request_duration_seconds_bucket{decision="allowed"}) > 0.010`).

**Likely cause**: this is the gateway's core performance SLO being
violated. Common causes: (a) Redis latency increased (network, a noisy
neighbor, Redis under memory pressure), (b) the spend-event write buffer
is backing up and the writer goroutine is contending for CPU with
request handling, (c) GC pressure from a traffic spike, or (d) the
gateway is running on under-provisioned hardware for current load.

**Response**:
1. Check Redis latency directly: `redis-cli --latency -h <redis-host>`.
   If Redis itself is slow, that's the root cause — investigate Redis
   memory/CPU, not the gateway.
2. Check `budget_governor_gateway_spend_write_dropped_total` — if it's
   climbing, the async writer is falling behind, which doesn't directly
   cause hot-path latency (writes are async) but indicates the process
   is under more load than it can sustain, which often correlates with
   GC-driven latency too.
3. Check the gateway's CPU/memory (via `node-exporter`/container stats).
   Horizontal scaling (more gateway replicas behind the load balancer)
   is the standard fix if the process itself is saturated — the gateway
   is stateless and designed to scale this way (see
   `docs/ARCHITECTURE.md#scaling-considerations`).
4. If this fires during a load test (`make bench`), compare against the
   documented baseline in `loadtest/results/` — a regression versus
   baseline points at a recent code change, not infrastructure.

### RejectionRateHigh

**Fires when**: more than 20% of gateway requests are rejected
(budget-exceeded) over a 10-minute window.

**Likely cause**: (a) one or more tenants are consistently exceeding
their configured budgets (under-provisioned budgets relative to real
usage), (b) a client-side bug causing retry storms against an
already-rejected budget, or (c) a genuine incident (compromised API key
being used for abusive volume).

**Response**:
1. Check `budget_governor_gateway_rejections_total` broken out by
   `reason` and `tenant_id` (Gateway Health dashboard) to see whether
   this is concentrated in one tenant or spread across many.
2. If concentrated in one tenant: check whether their budget
   configuration matches their actual plan/expected usage — this may
   simply need a budget increase via `POST /v1/budgets/{tenant_id}`.
3. If it looks like a retry storm (many rejections in a tight loop from
   one agent), the client should back off; check `Retry-After`-style
   guidance is being respected upstream.
4. If it looks like abuse (unexpected tenant/agent, unusual model
   selection, geographic/timing anomalies), revoke the API key
   (`revoked_at` set via the control plane) and issue a new one after
   investigation.

### ProviderErrorSpike

**Fires when**: upstream provider 5xx error rate is more than 3x its
trailing-hour baseline for 2 minutes.

**Likely cause**: (a) an actual provider outage/degradation (check the
provider's public status page), (b) the gateway is sending malformed
requests that the provider is rejecting as 5xx (rare — most malformed-
request errors are 4xx, but worth ruling out after a gateway deploy),
or (c) rate-limit exhaustion at the provider surfacing as 5xx in some
edge cases.

**Response**:
1. Check `budget_governor_gateway_provider_errors_total` by `provider`
   to identify which upstream (OpenAI/Anthropic) is affected.
2. Check the provider's public status page. If it's a confirmed outage,
   this is not actionable on the gateway side beyond monitoring — the
   gateway's retry-with-backoff (see `internal/provider`) and the
   circuit-breaker-style fast-fail behavior already limit blast radius
   (see the failure-modes table in `docs/ARCHITECTURE.md`).
3. If it's isolated to requests just after a gateway deploy, check
   recent changes to `internal/provider/openai.go` /
   `anthropic.go` for a malformed-request regression.
4. If sustained, consider whether the affected provider's traffic should
   be temporarily routed to the alternate provider (a policy-engine-
   level decision, not currently automated) or to the
   mock provider for non-critical/demo tenants.
