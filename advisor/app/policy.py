"""Local, advisory-side guardrails applied to LLM output BEFORE it's
returned to the gateway.

This is NOT the deterministic policy engine referenced throughout
docs/DECISIONS.md ADR-003 -- that authority lives entirely in
gateway/internal/policy (Go), which independently re-verifies anything
this service returns (e.g. re-checking a switch_model suggestion against
the real pricing table itself) before ever acting on it. What lives here
is narrower and purely defensive: even though Pydantic (schemas.py)
already rejects structurally invalid output, an LLM can return output
that is structurally VALID but substantively wrong -- e.g. "suggesting"
a switch to a model that is not actually cheaper, or duplicate
suggestions of the same type. Filtering those out here means the gateway
sees a clean, deduplicated, pricing-sane suggestion list, while the
gateway's own policy engine remains the only component whose approval is
load-bearing for the actual budget decision.
"""

from __future__ import annotations

from providers.pricing import cheaper_alternatives
from app.schemas import Suggestion, SuggestionType


def apply_guardrails(requested_model: str, suggestions: list[Suggestion]) -> list[Suggestion]:
    seen_types: set[SuggestionType] = set()
    out: list[Suggestion] = []

    for s in suggestions:
        if s.type in seen_types:
            continue  # one suggestion per type, keep the LLM's highest-ranked

        if s.type == SuggestionType.switch_model:
            valid_targets = set(cheaper_alternatives(requested_model))
            if s.suggested_model not in valid_targets:
                continue  # LLM claimed a "cheaper" model that isn't actually cheaper

        out.append(s)
        seen_types.add(s.type)

    return out
