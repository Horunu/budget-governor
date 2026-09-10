# Engineering Decision Log

ADR-style entries. Each records the decision, the context, and what we gave
up by choosing it.

---

## ADR-001: Go for the gateway, Python for everything else

**Decision**: The gateway (hot path: auth, budget enforcement, proxying,
streaming) is Go. The control plane, cost advisor, and reconciliation job
are Python.

**Context**: The gateway has a hard ≤10ms p99 overhead budget and needs to
handle high concurrent fan-out (5,000+ req/s target) with predictable tail
latency. Go's goroutines give cheap concurrency without an async runtime
to reason about, its GC pauses are sub-millisecond in modern versions, and
`net/http` + a Redis client with pipelining gets us a fast, boring hot
path. Everything else in this system — talking to LLM provider SDKs,
schema-validating LLM output, orchestrating a scheduled reconciliation
job, CRUD over Postgres — is I/O-bound, iterates faster in Python, and has
much better library support for LLM provider clients (`openai`,
`anthropic` are Python/TS-first).

**Alternative considered**: All-Go, including the control plane and
advisor, for a single-language repo. Rejected because the advisor's job is
fundamentally "call an LLM and validate its output against a schema" —
Python's `pydantic` + `instructor`-style validation is a better fit than
hand-rolling the equivalent in Go, and the control plane's CRUD surface
doesn't benefit from Go's performance characteristics enough to justify
losing Python's faster iteration there.

**Cost of this choice**: two languages, two dependency ecosystems, two
sets of CI tooling. Mitigated by keeping the *contract* between them
narrow and boring (HTTP + JSON, Postgres as the shared system of record)
rather than reaching for gRPC/protobuf — see ADR-006.

---

## ADR-002: Redis + Lua scripts for the token bucket, not an in-process or DB-backed counter

**Decision**: Budget state (token bucket per tenant/agent/task) lives in
Redis, mutated exclusively through a single atomic Lua script
(`checkAndDecrement.lua`) executed via `EVALSHA`.

**Context**: The gateway must be horizontally scalable (multiple replicas
behind a load balancer) while enforcing a *shared* budget ceiling across
all replicas. An in-process counter would be trivially fast but wrong the
moment there's more than one gateway replica — two replicas could each
independently believe there's budget for the same tenant and jointly
overspend. A Postgres-backed counter would be correct but violates the
"no synchronous DB write on the hot path" constraint and adds
5-50ms+ of tail latency depending on connection pool contention.

Redis gives us: (1) sub-millisecond round trips for a single-command Lua
script, (2) atomicity for free — Redis executes a script as one
indivisible operation, so concurrent requests for the same key cannot
interleave a read and a write, which is exactly the race that would let
two nearly-simultaneous requests both "see" the same remaining budget and
both succeed when only one should. Doing the equivalent with
`GET`/compute/`SET` from the client side would have a check-then-act race
under concurrency.

**Alternative considered**: `redis-cell` or a generic rate-limiting
library. Rejected because our bucket isn't a pure rate limiter — it needs
three nested scopes (tenant/agent/task) checked and decremented
atomically *together* in one round trip, and needs to support both
token-denominated and dollar-denominated limits with different refill
semantics. A hand-written script gives full control over that logic
without forcing three separate round trips (which would reintroduce a
cross-bucket race).

**Cost of this choice**: Redis becomes a hard dependency on the request
path — if it's down, budget checks cannot happen. We chose to fail closed
(reject requests) rather than fail open (allow unmetered spend); see the
failure-modes table in `docs/ARCHITECTURE.md`. This is a deliberate
trade: availability of the gateway is sacrificed to protect the
budget-enforcement guarantee, because an ungoverned LLM bill is the exact
failure this system exists to prevent.

---

## ADR-003: The cost advisor is async-only, gated by a deterministic policy engine, never on the hot path

**Decision**: The advisor service is invoked by the gateway *only* after a
deterministic budget check has already failed (budget pressure). Its
output is a set of ranked, confidence-scored suggestions — never a
decision. The gateway's policy engine (deterministic Go code,
`gateway/internal/policy`) is the only component that turns a suggestion
into an accept/reject/partial-allowance outcome, and it does so with pure
functions of (suggestion, remaining budget, policy config) — same inputs,
same output, every time.

**Context**: An LLM call is (a) slow — hundreds of milliseconds to
seconds, incompatible with any hot-path latency budget, (b)
non-deterministic — the same inputs can produce different outputs run to
run, which is unacceptable for something gating spend decisions, and (c)
itself a cost — consulting an LLM on every request to save money on LLM
calls is self-defeating. Restricting it to the pressure branch means it
fires on a small minority of requests by construction, and keeping the
policy engine as the sole decision-maker means the system's spend
guarantees don't depend on trusting a model's judgment.

**Alternative considered**: Let the advisor's suggestion be auto-applied
directly. Rejected — this is the resume bullet's "LLM-as-advisor... gating
all decisions [via] a deterministic policy engine" requirement, and it's
also just good engineering: an LLM should never be the sole safety
mechanism for something with real dollar consequences.

**Cost of this choice**: the advisor's suggestions can be stale by the
time the policy engine acts on them (though the window is small — single
request lifecycle) and the policy engine's rules necessarily lag behind
whatever creative suggestions the LLM might generate, since only
suggestion *types* the policy engine already knows how to evaluate
(`switch_model`, `reduce_max_tokens`, `summarize_first`,
`reduce_tool_calls`) can ever be acted on. A suggestion type the policy
engine doesn't recognize is logged and ignored, not guessed at.

---

## ADR-004: Reconciliation runs as a separate scheduled job, not inline with the gateway

**Decision**: Drift detection between gateway-observed spend and
provider-billed spend runs as an independent, schedulable Python job
(`reconciliation/job.py`), not as a gateway background thread.

**Context**: Provider billing/usage APIs are eventually consistent —
OpenAI's and Anthropic's usage/cost reporting can lag real-time by hours,
and cost data in particular can take up to 24h to settle (see the
provider docs cited in `reconciliation/providers/`). Reconciliation is
therefore fundamentally a batch process over a trailing, *closed* time
window (e.g. "yesterday, UTC"), not something that can run continuously
against a still-open window without producing false-positive drift from
data that simply hasn't arrived yet.

Decoupling it from the gateway process also means: it can run on a
completely different schedule/infra (cron, Kubernetes CronJob, Airflow,
whatever the deployment target uses) without needing to coordinate with
gateway deploys, restarts, or scaling events, and a slow/failing provider
usage API call cannot ever contend with gateway resources.

**Alternative considered**: A long-lived background goroutine inside the
gateway. Rejected because it would mean every gateway replica either
independently double-runs reconciliation or requires leader election to
avoid that — unnecessary complexity for something that only needs to run
once a day per tenant.

**Cost of this choice**: reconciliation results are not real-time; there
is an inherent lag between an overspend event actually happening and the
system detecting billing drift for it. This is acceptable because
real-time protection against overspend is already the job of the
token-bucket enforcement layer — reconciliation exists to catch *drift*
(estimation error, provider-side discrepancies, mid-stream failures we
mis-accounted for), not to be the primary spend guard.

---

## ADR-005: Auth uses bcrypt-hashed API keys + scopes, not OAuth2/JWT

**Decision**: Tenants authenticate to both the gateway and the control
plane using a static API key (`bg_live_<random>`), stored server-side only
as a bcrypt hash, carrying one or more scopes (`admin`, `agent`,
`viewer`) checked per-endpoint. No OAuth2 authorization-code flow, no
externally-issued JWTs.

**Context**: This system's clients are *other services* (an org's
LLM-calling agents, CI jobs, internal dashboards) — machine-to-machine,
not end-user browser sessions. OAuth2's redirect/consent flows solve a
problem (a human delegating access to a third party) that doesn't exist
here. A static, revocable, scoped API key is the standard machine-to-
machine credential (this is exactly what OpenAI, Anthropic, Stripe, and
most infra APIs do) and is far simpler to implement correctly than
running an OAuth2 authorization server.

We do borrow JWT-shaped thinking for the *internal* control-plane-to-
gateway trust boundary where useful, but the externally-presented
credential is the API key.

**Why bcrypt over a plain hash (SHA-256) or reversible encryption**: the
key itself functions as a bearer secret equivalent to a password; bcrypt's
deliberate slowness and built-in salting protects against offline
brute-force if the `api_keys` table were ever exfiltrated, which a fast
hash (even salted SHA-256) does not. The cost is bcrypt's ~50-100ms verify
time — irrelevant here because the gateway never bcrypt-verifies on the
hot path (see ADR below / ARCHITECTURE.md auth caching); it's paid once on
cache-miss/key-rotation, not per-request.

**Why a `key_prefix` index**: bcrypt hashes aren't comparable/indexable by
the database, so a naive implementation would have to bcrypt-verify
against every active key in the table on every auth check — O(n) in tenant
count. We store the first 12 characters of the key in plaintext as an
indexed `key_prefix` column (collision-safe in practice given the random
key space) purely to narrow the lookup to one row before the O(1) bcrypt
verify.

**Alternative considered**: Opaque bearer tokens issued via an OAuth2
client-credentials grant. This is architecturally closer to what we did
(client-credentials is also machine-to-machine) but adds a token-issuance
and refresh-flow surface with no benefit over a long-lived, revocable,
directly-issued key for this system's actual clients. Documented as a
reasonable future evolution if third-party (non-tenant-owned) integrations
are ever added.

**Cost of this choice**: no built-in short-lived-token expiry/refresh
semantics; revocation is immediate-effect but requires the tenant to
explicitly rotate. Mitigated by the `api_keys.revoked_at` column and the
control plane's key-rotation endpoint, plus the 60s auth-cache TTL
bounding how long a revoked key can still work in the worst case.

---

## ADR-006: HTTP+JSON between services, not gRPC

**Decision**: Gateway ↔ advisor and gateway ↔ control plane (auth
refresh) communicate over plain HTTP + JSON, not gRPC/protobuf, despite
the `proto/` directory existing in the target layout.

**Context**: Every cross-service call in this system is either
low-frequency (advisor: only on budget pressure; auth cache refresh: every
60s) or genuinely off the hot path. gRPC's main wins — binary framing,
strict schemas, multiplexed streaming — don't move the needle at this call
volume, and JSON keeps the advisor's contract trivially inspectable/
curl-able during development and in the smoke test. The `proto/` directory
is kept empty with a `README.md` explaining this so the target layout's
intent is documented rather than silently dropped.

**Alternative considered**: gRPC for the gateway-advisor contract
specifically, since it's the one place a schema-first contract has some
appeal. Deferred — if this system grows additional advisor-like services
with tighter latency needs, this is the first thing to revisit.

---

## ADR-007: Plain, ordered SQL migrations in a shared `migrations/` directory, not Alembic

**Decision**: Both the control plane and the gateway's auth cache read
from the same Postgres schema, defined by numbered plain-SQL files in
`migrations/` (`0001_init.sql`, `0002_...sql`, ...), applied in order by a
small Python runner (`scripts/migrate.py`) rather than Alembic.

**Context**: The schema is shared across two runtimes (Go gateway reads
`api_keys`/`tenants`; Python control plane owns writes to nearly every
table; the reconciliation job reads/writes `reconciliation_*` tables).
Alembic's migration-as-Python-code model is natural when exactly one
Python codebase owns the schema, but here it would create an awkward
split: either the Go side has no visibility into schema history at all
(migrations live entirely inside `controlplane/`, invisible from
`gateway/`), or we'd need to duplicate Alembic tooling access into a
service that has no other Python dependency. Plain, numbered `.sql` files
are readable and applicable from any language/tool, keep the schema
definition in one place both services can point at, and avoid asking
whoever's touching the Go gateway to understand Alembic's revision graph
to see what `api_keys` looks like.

**Alternative considered**: Alembic in `controlplane/`, with the schema
treated as "owned" by the control plane and the gateway only ever reading
it. This is defensible and arguably more idiomatic Python — noted here so
the trade is explicit rather than assumed. We chose the shared-SQL path
because this project's whole premise is a polyglot system with a shared
system of record, and the migrations directory in the target layout was
already specified as the shared location.

**Cost of this choice**: no auto-generated migrations from ORM model
diffs (SQLAlchemy models in `controlplane/app/db/models.py` are kept in
sync with the SQL by hand/review, not by `alembic revision --autogenerate`).
For a schema this size, the trade is worth the simplicity.
