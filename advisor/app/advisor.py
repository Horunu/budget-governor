"""The advisor's core entrypoint: call an LLM (or the mock), validate its
output, apply local guardrails, and return ranked suggestions. Called by
the gateway ONLY when a request is about to be throttled -- see
docs/DECISIONS.md ADR-003. This function never raises: any failure
(network error, malformed LLM output, schema validation failure) is
logged and swallowed into an empty suggestion list, per the "advisor
never crashes" requirement -- the gateway's policy engine already has a
deterministic fallback (the original budget rejection) for exactly this
case.
"""

from __future__ import annotations

import json
import logging

from pydantic import ValidationError

from app.llm_client import LLMClient
from app.policy import apply_guardrails
from app.schemas import AdviseRequest, AdviseResponse, LLMSuggestionsPayload

logger = logging.getLogger("advisor")


async def get_suggestions(request: AdviseRequest, llm_client: LLMClient) -> AdviseResponse:
    try:
        raw_text = await llm_client.complete(request)
    except Exception as exc:  # noqa: BLE001 -- deliberately broad, see module docstring
        logger.warning("llm call failed, returning no suggestions: %s", exc)
        return AdviseResponse(suggestions=[])

    suggestions = parse_llm_output(raw_text)
    if not suggestions:
        return AdviseResponse(suggestions=[])

    guarded = apply_guardrails(request.requested_model, suggestions)
    return AdviseResponse(suggestions=guarded)


def parse_llm_output(raw_text: str) -> list:
    """Strictly validates raw LLM text against LLMSuggestionsPayload.
    Returns an empty list on ANY parsing or validation failure -- never
    raises. This is the one function the "schema validation failure ->
    empty suggestions" test exercises directly.
    """
    try:
        data = json.loads(raw_text)
    except (json.JSONDecodeError, TypeError) as exc:
        logger.warning("llm output was not valid JSON: %s", exc)
        return []

    try:
        payload = LLMSuggestionsPayload.model_validate(data)
    except ValidationError as exc:
        logger.warning("llm output failed schema validation: %s", exc)
        return []

    return payload.suggestions
