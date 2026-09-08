"""Anthropic organization Cost Report API client.

Shape verified against https://platform.claude.com/docs/en/manage-claude/usage-cost-api
(retrieved 2026-09-10):

    GET https://api.anthropic.com/v1/organizations/cost_report
      ?starting_at=<RFC3339, required>
      &ending_at=<RFC3339>
      &group_by[]=workspace_id
      headers: anthropic-version: 2023-06-01

    {
      "data": [
        {
          "start_time": "...", "end_time": "...",
          "results": [
            {"workspace_id": "wrkspc_abc", "amount": "12.34", "currency": "usd", ...}
          ]
        }
      ],
      "has_more": false,
      "next_page": null
    }

Requires an Admin API key (`sk-ant-admin01-...`) or an org:admin-scoped
OAuth token, set via ANTHROPIC_ADMIN_KEY. Cost values are documented as
decimal USD strings; this client parses them as float.

Note: Priority Tier spend is excluded from this endpoint per Anthropic's
own docs -- a tenant using Priority Tier would need the usage endpoint's
service_tier=priority filter added to fully reconcile. Not implemented
here; see docs/BUILD_SUMMARY.md for the list of documented gaps.
"""

from __future__ import annotations

from datetime import datetime

import httpx

from usage_providers.base import UsageProvider


class AnthropicUsageProvider(UsageProvider):
    def __init__(self, admin_key: str, base_url: str = "https://api.anthropic.com/v1", timeout_s: float = 30.0):
        self.admin_key = admin_key
        self.base_url = base_url.rstrip("/")
        self.timeout_s = timeout_s

    async def get_cost_usd(self, account_ref: str, window_start: datetime, window_end: datetime) -> float:
        total = 0.0
        page: str | None = None
        async with httpx.AsyncClient(timeout=self.timeout_s) as client:
            while True:
                params: dict = {
                    "starting_at": window_start.isoformat(),
                    "ending_at": window_end.isoformat(),
                    "group_by[]": "workspace_id",
                }
                if page:
                    params["page"] = page

                resp = await client.get(
                    f"{self.base_url}/organizations/cost_report",
                    headers={
                        "x-api-key": self.admin_key,
                        "anthropic-version": "2023-06-01",
                    },
                    params=params,
                )
                resp.raise_for_status()
                body = resp.json()

                for bucket in body.get("data", []):
                    for result in bucket.get("results", []):
                        if account_ref and result.get("workspace_id") != account_ref:
                            continue
                        total += float(result.get("amount", 0.0))

                if not body.get("has_more"):
                    break
                page = body.get("next_page")
                if not page:
                    break

        return total
