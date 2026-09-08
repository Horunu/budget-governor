"""Common interface every provider usage/cost client implements.

Reconciliation compares gateway-observed spend (from Postgres
spend_events, real-time) against what the upstream provider itself
reports as billed (this interface) -- and the two are NOT expected to
match instantly: provider billing APIs are eventually consistent, often
settling hours to a day after the underlying usage. This is why
job.py always reconciles a trailing, CLOSED window (e.g. "yesterday,
UTC") rather than "right now" -- see docs/DECISIONS.md ADR-004.
"""

from __future__ import annotations

from abc import ABC, abstractmethod
from datetime import datetime


class UsageProvider(ABC):
    @abstractmethod
    async def get_cost_usd(self, account_ref: str, window_start: datetime, window_end: datetime) -> float:
        """Returns the provider-reported cost in USD for account_ref
        (an org/project/workspace identifier, provider-specific -- see
        tenants.openai_org_id / tenants.anthropic_workspace_id) over
        [window_start, window_end).
        """
        raise NotImplementedError
