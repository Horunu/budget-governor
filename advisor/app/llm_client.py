"""LLM clients the advisor can call. MockLLMClient is deterministic and
makes no network calls -- it's what docker-compose runs by default, and
what makes `make smoke` work without any provider API keys. The real
clients (OpenAIAdvisorClient, AnthropicAdvisorClient) are fully
implemented against the same request/response shapes as
gateway/internal/provider, so setting OPENAI_API_KEY / ANTHROPIC_API_KEY
is a one-line config change (see ADVISOR_LLM_PROVIDER in .env.example),
not a code change.
"""

from __future__ import annotations

import json
from abc import ABC, abstractmethod
from typing import Protocol

import httpx

from providers.pricing import cheaper_alternatives, cost_usd, price
from app.schemas import AdviseRequest


class LLMClient(Protocol):
    async def complete(self, request: AdviseRequest) -> str:
        """Returns the raw text the LLM produced. May be malformed --
        the caller (advisor.py) is responsible for validating it.
        """
        ...


SYSTEM_PROMPT = """You are a cost-optimization advisor for an LLM gateway. \
A request was just throttled by a deterministic budget enforcer. Given the \
request context and recent call history, suggest 1-3 RANKED ways to reduce \
cost, most impactful first. You are NEVER the decision-maker -- a separate \
deterministic policy engine decides whether to act on your suggestions, so \
be honest about uncertainty via the confidence field rather than always \
claiming high confidence.

Respond with ONLY a JSON object of this exact shape, no prose:
{"suggestions": [
  {"type": "switch_model", "suggested_model": "<model id>", "confidence": 0.0-1.0, "risk_note": "<string>"},
  {"type": "reduce_max_tokens", "suggested_max_tokens": <int>, "confidence": 0.0-1.0, "risk_note": "<string>"},
  {"type": "summarize_first", "confidence": 0.0-1.0, "risk_note": "<string>"},
  {"type": "reduce_tool_calls", "confidence": 0.0-1.0, "risk_note": "<string>"}
]}
Only include suggestion types that are actually applicable."""


def build_user_prompt(request: AdviseRequest) -> str:
    recent = "\n".join(
        f"  - {c.model}: {c.tokens_in} in / {c.tokens_out} out, ${c.cost_usd:.6f}"
        for c in request.recent_calls[-10:]
    ) or "  (no recent call history available)"
    alternatives = cheaper_alternatives(request.requested_model)
    return f"""Requested model: {request.requested_model}
Estimated tokens for this call: {request.estimated_tokens}
Remaining budget: {request.remaining_tokens:.0f} tokens, ${request.remaining_usd:.4f}
Budget scope that rejected this call: {request.failed_scope}
Known cheaper alternatives in the same provider family: {alternatives or "none"}
Recent calls for this agent:
{recent}"""


class MockLLMClient:
    """Deterministic, network-free "LLM": derives suggestions from the
    same pricing table and request context a real LLM would be shown, so
    the output is realistic and internally consistent (e.g. it never
    "suggests" a model that isn't actually cheaper), without needing an
    API key. This is intentionally a heuristic, not a language model --
    it exists to make the advisory *pipeline* (prompting, parsing,
    validation, policy evaluation) fully exercisable offline, not to
    demonstrate LLM reasoning quality.
    """

    async def complete(self, request: AdviseRequest) -> str:
        suggestions = []

        alternatives = cheaper_alternatives(request.requested_model)
        if alternatives:
            requested_price = price(request.requested_model)
            # Prefer the cheapest alternative that still plausibly fits
            # the remaining budget for this call's token estimate.
            best = min(alternatives, key=lambda m: price(m).output if price(m) else float("inf"))
            projected_cost = cost_usd(best, request.estimated_tokens, 0, 0)
            confidence = 0.85 if projected_cost <= request.remaining_usd or request.remaining_usd == 0 else 0.55
            savings_pct = 0.0
            if requested_price and requested_price.output > 0:
                best_price = price(best)
                if best_price:
                    savings_pct = 100 * (1 - best_price.output / requested_price.output)
            suggestions.append(
                {
                    "type": "switch_model",
                    "suggested_model": best,
                    "confidence": confidence,
                    "risk_note": f"~{savings_pct:.0f}% cheaper output tokens; likely lower reasoning quality on complex tasks.",
                }
            )

        if request.estimated_tokens > 0 and request.remaining_tokens > 0:
            reduced = int(request.remaining_tokens * 0.9)
            if 0 < reduced < request.estimated_tokens:
                suggestions.append(
                    {
                        "type": "reduce_max_tokens",
                        "suggested_max_tokens": reduced,
                        "confidence": 0.7,
                        "risk_note": "Response may be truncated for long-form answers.",
                    }
                )

        recent_growth = _detect_growing_prompt(request)
        if recent_growth:
            suggestions.append(
                {
                    "type": "summarize_first",
                    "confidence": 0.65,
                    "risk_note": "Conversation history is growing; summarizing older turns would reduce input tokens on future calls.",
                }
            )

        return json.dumps({"suggestions": suggestions[:3]})


def _detect_growing_prompt(request: AdviseRequest) -> bool:
    calls = request.recent_calls
    if len(calls) < 3:
        return False
    recent_window = [c.tokens_in for c in calls[-3:]]
    return recent_window[-1] > recent_window[0] * 1.5


class HTTPLLMClient(ABC):
    """Base for real-provider advisor clients. Calls are bounded by a
    short timeout (the gateway's own advisor call is bounded too, see
    gateway/internal/proxy.AdvisorClient) and any failure propagates as
    an exception -- advisor.py's caller treats that identically to a
    validation failure: zero suggestions, never a crash.
    """

    def __init__(self, api_key: str, base_url: str, model: str, timeout_s: float = 8.0):
        self.api_key = api_key
        self.base_url = base_url.rstrip("/")
        self.model = model
        self.timeout_s = timeout_s

    @abstractmethod
    async def complete(self, request: AdviseRequest) -> str: ...


class OpenAIAdvisorClient(HTTPLLMClient):
    """Uses the Chat Completions API shape (see
    gateway/internal/provider/openai.go for the same shapes verified
    against current docs) with response_format json_object for reliable
    parsing.
    """

    async def complete(self, request: AdviseRequest) -> str:
        async with httpx.AsyncClient(timeout=self.timeout_s) as client:
            resp = await client.post(
                f"{self.base_url}/chat/completions",
                headers={"Authorization": f"Bearer {self.api_key}", "Content-Type": "application/json"},
                json={
                    "model": self.model,
                    "messages": [
                        {"role": "system", "content": SYSTEM_PROMPT},
                        {"role": "user", "content": build_user_prompt(request)},
                    ],
                    "response_format": {"type": "json_object"},
                    "max_completion_tokens": 500,
                },
            )
            resp.raise_for_status()
            body = resp.json()
            return body["choices"][0]["message"]["content"]


class AnthropicAdvisorClient(HTTPLLMClient):
    """Uses the Messages API shape (see
    gateway/internal/provider/anthropic.go for the same shapes verified
    against current docs)."""

    async def complete(self, request: AdviseRequest) -> str:
        async with httpx.AsyncClient(timeout=self.timeout_s) as client:
            resp = await client.post(
                f"{self.base_url}/messages",
                headers={
                    "x-api-key": self.api_key,
                    "anthropic-version": "2023-06-01",
                    "Content-Type": "application/json",
                },
                json={
                    "model": self.model,
                    "max_tokens": 500,
                    "system": SYSTEM_PROMPT,
                    "messages": [{"role": "user", "content": build_user_prompt(request)}],
                },
            )
            resp.raise_for_status()
            body = resp.json()
            return "".join(block.get("text", "") for block in body.get("content", []) if block.get("type") == "text")
