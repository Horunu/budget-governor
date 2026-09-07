"""Pydantic schemas for the advisor's request/response contract with the
gateway, and for strictly validating whatever the LLM returns.

IMPORTANT: nothing in this file, or anywhere in this service, makes a
budget decision. The advisor only ever returns ranked, confidence-scored
SUGGESTIONS. The gateway's deterministic policy engine
(gateway/internal/policy) is the sole authority that turns a suggestion
into an accept/reject/partial-allowance outcome -- see
docs/DECISIONS.md ADR-003.
"""

from __future__ import annotations

from enum import Enum

from pydantic import BaseModel, Field, field_validator


class SuggestionType(str, Enum):
    switch_model = "switch_model"
    reduce_max_tokens = "reduce_max_tokens"
    summarize_first = "summarize_first"
    reduce_tool_calls = "reduce_tool_calls"


class Suggestion(BaseModel):
    """One ranked cost-reduction suggestion. Strictly validated against
    whatever the LLM returns -- see advisor.py's parse_llm_output, which
    discards the entire response (falling back to zero suggestions,
    never a crash) if it doesn't conform to this shape.
    """

    type: SuggestionType
    suggested_model: str | None = None
    suggested_max_tokens: int | None = Field(default=None, gt=0)
    confidence: float = Field(ge=0.0, le=1.0)
    risk_note: str = Field(default="", max_length=500)


class LLMSuggestionsPayload(BaseModel):
    """The exact shape the advisor asks the LLM to produce, and the
    schema its raw text output is validated against. Anything that
    doesn't parse into this -- malformed JSON, an unrecognized field,
    a confidence outside [0,1], too many suggestions -- is treated as a
    validation failure by advisor.py, never as a crash.
    """

    suggestions: list[Suggestion] = Field(default_factory=list, max_length=3)

    @field_validator("suggestions")
    @classmethod
    def _validate_cross_fields(cls, suggestions: list[Suggestion]) -> list[Suggestion]:
        valid: list[Suggestion] = []
        for s in suggestions:
            if s.type == SuggestionType.switch_model and not s.suggested_model:
                continue  # drop: switch_model without a target model is meaningless
            if s.type == SuggestionType.reduce_max_tokens and not s.suggested_max_tokens:
                continue  # drop: same, for reduce_max_tokens
            valid.append(s)
        return valid


class CallRecord(BaseModel):
    model: str
    tokens_in: int
    tokens_out: int
    cost_usd: float


class AdviseRequest(BaseModel):
    tenant_id: str
    agent_id: str = ""
    task_id: str = ""
    requested_model: str
    estimated_tokens: int
    remaining_tokens: float = 0
    remaining_usd: float = 0
    failed_scope: str = ""
    recent_calls: list[CallRecord] = Field(default_factory=list)


class AdviseResponse(BaseModel):
    suggestions: list[Suggestion] = Field(default_factory=list)
