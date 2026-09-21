from __future__ import annotations

import os

from app.llm_client import AnthropicAdvisorClient, HTTPLLMClient, LLMClient, MockLLMClient, OpenAIAdvisorClient


def build_llm_client() -> LLMClient:
    """Selects the advisor's LLM backend from ADVISOR_LLM_PROVIDER
    ("mock" | "openai" | "anthropic"; defaults to "mock" so the whole
    system runs with no API keys). Set DEMO_MODE=true to force the mock
    client regardless of ADVISOR_LLM_PROVIDER — safe for demos without
    any API keys. See .env.example for the full set of variables.
    """
    if os.environ.get("DEMO_MODE", "").lower() in ("1", "true", "yes"):
        return MockLLMClient()

    provider = os.environ.get("ADVISOR_LLM_PROVIDER", "mock").lower()

    if provider == "openai":
        api_key = os.environ.get("OPENAI_API_KEY", "")
        if not api_key:
            return MockLLMClient()
        return OpenAIAdvisorClient(
            api_key=api_key,
            base_url=os.environ.get("OPENAI_BASE_URL", "https://api.openai.com/v1"),
            model=os.environ.get("ADVISOR_OPENAI_MODEL", "gpt-4o-mini"),
        )

    if provider == "anthropic":
        api_key = os.environ.get("ANTHROPIC_API_KEY", "")
        if not api_key:
            return MockLLMClient()
        return AnthropicAdvisorClient(
            api_key=api_key,
            base_url=os.environ.get("ANTHROPIC_BASE_URL", "https://api.anthropic.com/v1"),
            model=os.environ.get("ADVISOR_ANTHROPIC_MODEL", "claude-haiku-4-5"),
        )

    return MockLLMClient()


__all__ = ["build_llm_client", "HTTPLLMClient"]
