# Budget Governor

A multi-tenant LLM-call gateway that enforces per-tenant, per-agent, and
per-task token/dollar budgets in real time — with an LLM-as-advisor cost
optimizer gated by a deterministic policy engine, a drift-detecting
reconciliation pipeline against provider billing, and a full observability
stack that alerts before the bill arrives, not after.

> Status: this README is being finalized as the build progresses (see
> `docs/BUILD_SUMMARY.md` once present for current state). Full quickstart,
> architecture diagram, and resume-bullet mapping land in Phase 7 of the
> build.

See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for the system design,
[`docs/DECISIONS.md`](docs/DECISIONS.md) for engineering rationale, and
[`docs/RESUME_BULLETS.md`](docs/RESUME_BULLETS.md) for what this project
demonstrates and where.
