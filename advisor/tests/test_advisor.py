from __future__ import annotations

import pytest

from app.advisor import get_suggestions, parse_llm_output
from app.llm_client import MockLLMClient
from app.schemas import AdviseRequest, CallRecord, Suggestion, SuggestionType


def make_request(**overrides) -> AdviseRequest:
    defaults = dict(
        tenant_id="tenant-acme",
        agent_id="agent-1",
        requested_model="claude-opus-5",
        estimated_tokens=500,
        remaining_tokens=0,
        remaining_usd=0,
        failed_scope="tenant",
        recent_calls=[],
    )
    defaults.update(overrides)
    return AdviseRequest(**defaults)


# --- parse_llm_output: schema validation ------------------------------------


def test_parse_llm_output_valid_json():
    raw = '{"suggestions": [{"type": "switch_model", "suggested_model": "claude-haiku-4-5", "confidence": 0.8, "risk_note": "lower quality"}]}'
    suggestions = parse_llm_output(raw)
    assert len(suggestions) == 1
    assert suggestions[0].type == SuggestionType.switch_model
    assert suggestions[0].suggested_model == "claude-haiku-4-5"


def test_parse_llm_output_malformed_json_returns_empty():
    assert parse_llm_output("not json at all {{{") == []


def test_parse_llm_output_wrong_shape_returns_empty():
    # Valid JSON, but `suggestions` is a string instead of a list.
    assert parse_llm_output('{"suggestions": "nope"}') == []


def test_parse_llm_output_confidence_out_of_range_drops_field_level():
    # confidence > 1.0 fails Suggestion's field constraint -> the whole
    # payload fails validation (Pydantic doesn't partially-accept a list
    # item), so this is treated as total validation failure.
    raw = '{"suggestions": [{"type": "switch_model", "suggested_model": "x", "confidence": 1.5}]}'
    assert parse_llm_output(raw) == []


def test_parse_llm_output_unknown_type_returns_empty():
    raw = '{"suggestions": [{"type": "rewrite_the_universe", "confidence": 0.9}]}'
    assert parse_llm_output(raw) == []


def test_parse_llm_output_too_many_suggestions_returns_empty():
    one = '{"type": "summarize_first", "confidence": 0.5}'
    raw = '{"suggestions": [' + ",".join([one] * 5) + "]}"
    assert parse_llm_output(raw) == []


def test_parse_llm_output_switch_model_without_target_is_dropped_not_failed():
    # Structurally valid, but semantically incomplete -- the
    # cross-field validator drops just this suggestion rather than
    # failing the whole payload.
    raw = '{"suggestions": [{"type": "switch_model", "confidence": 0.9}]}'
    assert parse_llm_output(raw) == []


def test_parse_llm_output_empty_suggestions_list_is_valid():
    assert parse_llm_output('{"suggestions": []}') == []


# --- get_suggestions: end-to-end with a fake client -------------------------


class RaisingLLMClient:
    async def complete(self, request):
        raise ConnectionError("upstream LLM unreachable")


class MalformedLLMClient:
    async def complete(self, request):
        return "I cannot comply with that request."


class ValidLLMClient:
    async def complete(self, request):
        return (
            '{"suggestions": [{"type": "switch_model", "suggested_model": "claude-sonnet-5", '
            '"confidence": 0.9, "risk_note": "moderate quality tradeoff"}]}'
        )


@pytest.mark.asyncio
async def test_get_suggestions_llm_failure_never_raises():
    resp = await get_suggestions(make_request(), RaisingLLMClient())
    assert resp.suggestions == []


@pytest.mark.asyncio
async def test_get_suggestions_malformed_output_never_raises():
    resp = await get_suggestions(make_request(), MalformedLLMClient())
    assert resp.suggestions == []


@pytest.mark.asyncio
async def test_get_suggestions_valid_output_passes_through_guardrails():
    # claude-sonnet-5 IS a real cheaper alternative to claude-opus-5 in
    # the pricing table, so this should survive apply_guardrails.
    resp = await get_suggestions(make_request(requested_model="claude-opus-5"), ValidLLMClient())
    assert len(resp.suggestions) == 1
    assert resp.suggestions[0].suggested_model == "claude-sonnet-5"


@pytest.mark.asyncio
async def test_get_suggestions_valid_but_not_actually_cheaper_is_filtered_by_guardrails():
    class OverclaimingLLMClient:
        async def complete(self, request):
            # claude-fable-5-1 is priced HIGHER than claude-opus-5.
            return '{"suggestions": [{"type": "switch_model", "suggested_model": "claude-fable-5-1", "confidence": 0.99}]}'

    resp = await get_suggestions(make_request(requested_model="claude-opus-5"), OverclaimingLLMClient())
    assert resp.suggestions == []


# --- MockLLMClient (the default, network-free backend) -----------------------


@pytest.mark.asyncio
async def test_mock_llm_client_suggests_a_real_cheaper_model():
    resp = await get_suggestions(make_request(requested_model="claude-opus-5"), MockLLMClient())
    assert len(resp.suggestions) >= 1
    switch = next((s for s in resp.suggestions if s.type == SuggestionType.switch_model), None)
    assert switch is not None
    assert switch.suggested_model in {"claude-sonnet-5", "claude-haiku-4-5"}


@pytest.mark.asyncio
async def test_mock_llm_client_no_alternatives_for_cheapest_model_in_family():
    # mock-small is already the cheapest mock-family model -- no
    # switch_model suggestion should be produced.
    resp = await get_suggestions(make_request(requested_model="mock-small"), MockLLMClient())
    assert all(s.type != SuggestionType.switch_model for s in resp.suggestions)


@pytest.mark.asyncio
async def test_mock_llm_client_suggests_reduce_max_tokens_when_budget_tight():
    resp = await get_suggestions(
        make_request(requested_model="mock-large", estimated_tokens=1000, remaining_tokens=200),
        MockLLMClient(),
    )
    reduce = next((s for s in resp.suggestions if s.type == SuggestionType.reduce_max_tokens), None)
    assert reduce is not None
    assert 0 < reduce.suggested_max_tokens < 1000


@pytest.mark.asyncio
async def test_mock_llm_client_detects_growing_conversation():
    calls = [
        CallRecord(model="mock-medium", tokens_in=100, tokens_out=50, cost_usd=0.01),
        CallRecord(model="mock-medium", tokens_in=150, tokens_out=50, cost_usd=0.01),
        CallRecord(model="mock-medium", tokens_in=400, tokens_out=50, cost_usd=0.01),
    ]
    resp = await get_suggestions(make_request(recent_calls=calls), MockLLMClient())
    assert any(s.type == SuggestionType.summarize_first for s in resp.suggestions)


@pytest.mark.asyncio
async def test_mock_llm_client_no_history_no_summarize_suggestion():
    resp = await get_suggestions(make_request(recent_calls=[]), MockLLMClient())
    assert all(s.type != SuggestionType.summarize_first for s in resp.suggestions)
