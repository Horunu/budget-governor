"""Shared LLM provider pricing table, used by the cost advisor and the
reconciliation job (drift detection needs to price gateway-observed
tokens the same way the gateway itself did).

This is the Python mirror of gateway/internal/provider/pricing.go --
same source, same verification date, same numbers. Keeping two files
instead of one shared-across-languages source is a deliberate,
documented trade (see docs/DECISIONS.md ADR-001 on the Go/Python split):
a single source of truth would need either a codegen step or a runtime
cross-language dependency, both heavier than re-stating one small table
in each language and reviewing them together on change.

Pricing verified 2026-09-10 against:
  - OpenAI: https://developers.openai.com/api/docs/pricing
  - Anthropic: https://platform.claude.com/docs/en/about-claude/pricing
    and https://platform.claude.com/docs/en/api/rate-limits (model roster
    cross-check)

Prices are USD per 1,000,000 tokens.
"""

from __future__ import annotations

from dataclasses import dataclass


@dataclass(frozen=True)
class PricePerMillion:
    input: float
    cached_input: float
    output: float


PRICING_TABLE: dict[str, PricePerMillion] = {
    # OpenAI
    "gpt-5.4": PricePerMillion(2.50, 0.25, 15.00),
    "gpt-5.4-mini": PricePerMillion(0.75, 0.075, 4.50),
    "gpt-5.4-nano": PricePerMillion(0.20, 0.02, 1.25),
    "gpt-5": PricePerMillion(1.25, 0.125, 10.00),
    "gpt-5-mini": PricePerMillion(0.25, 0.025, 2.00),
    "gpt-5-nano": PricePerMillion(0.05, 0.005, 0.40),
    "gpt-4.1": PricePerMillion(2.00, 0.50, 8.00),
    "gpt-4.1-mini": PricePerMillion(0.40, 0.10, 1.60),
    "gpt-4.1-nano": PricePerMillion(0.10, 0.025, 0.40),
    "gpt-4o": PricePerMillion(2.50, 1.25, 10.00),
    "gpt-4o-mini": PricePerMillion(0.15, 0.075, 0.60),
    "o1": PricePerMillion(15.00, 7.50, 60.00),
    "o3": PricePerMillion(2.00, 0.50, 8.00),
    "o3-mini": PricePerMillion(1.10, 0.55, 4.40),
    "o4-mini": PricePerMillion(1.10, 0.275, 4.40),
    # Anthropic
    "claude-fable-5-1": PricePerMillion(10.00, 0.25, 50.00),
    "claude-opus-5": PricePerMillion(5.00, 0.50, 25.00),
    "claude-sonnet-5": PricePerMillion(2.00, 0.20, 10.00),
    "claude-haiku-4-5": PricePerMillion(1.00, 0.10, 5.00),
    # Mock provider
    "mock-large": PricePerMillion(5.00, 0.50, 25.00),
    "mock-medium": PricePerMillion(1.00, 0.10, 5.00),
    "mock-small": PricePerMillion(0.15, 0.015, 0.60),
}


def normalize_model(model: str) -> str:
    if "/" in model:
        return model.split("/", 1)[1]
    return model


def price(model: str) -> PricePerMillion | None:
    return PRICING_TABLE.get(normalize_model(model))


def cost_usd(model: str, tokens_in: int, cached_input_tokens: int, tokens_out: int) -> float:
    p = price(model)
    if p is None:
        return 0.0
    uncached = max(tokens_in - cached_input_tokens, 0)
    return (
        uncached / 1_000_000 * p.input
        + cached_input_tokens / 1_000_000 * p.cached_input
        + tokens_out / 1_000_000 * p.output
    )


def model_family(model: str) -> str:
    m = normalize_model(model)
    if m.startswith("gpt-") or m.startswith("o1") or m.startswith("o3") or m.startswith("o4"):
        return "openai"
    if m.startswith("claude-"):
        return "anthropic"
    if m.startswith("mock-"):
        return "mock"
    return "unknown"


def cheaper_alternatives(model: str) -> list[str]:
    """Model IDs priced strictly below `model`'s output price, from the
    same provider family -- used by the advisor to only ever SUGGEST a
    switch that is actually cheaper (the gateway's policy engine
    independently re-verifies this before acting on it; see
    docs/DECISIONS.md ADR-003).
    """
    base = price(model)
    if base is None:
        return []
    family = model_family(model)
    return [
        name
        for name, p in PRICING_TABLE.items()
        if name != model and model_family(name) == family and p.output < base.output
    ]
