"""OpenAI organization Costs API client.

Shape verified against https://developers.openai.com/api/docs (retrieved
2026-09-10; see the research embedded in
gateway/internal/provider/pricing.go's sibling commit for full detail).

    GET https://api.openai.com/v1/organization/costs
      ?start_time=<unix seconds, required>
      &end_time=<unix seconds>
      &bucket_width=1d          (only granularity currently supported)
      &project_ids[]=<id>       (used here to scope to one tenant)
      &limit=<bucket count>

    {
      "object": "page",
      "data": [
        {
          "object": "bucket",
          "start_time": 1730419200,
          "end_time": 1730505600,
          "results": [
            {"object": "organization.costs.result", "amount": {"currency": "usd", "value": 12.34}, "project_id": "proj_abc", ...}
          ]
        }
      ],
      "has_more": false,
      "next_page": null
    }

Requires an Admin API key (`OPENAI_ADMIN_KEY`), distinct from a regular
project API key -- this endpoint is organization-scoped.

Note on the tenant<->OpenAI mapping: this system stores a single
`tenants.openai_org_id` column (see migrations/0001_init.sql), but
OpenAI's Costs API scopes cost data by PROJECT within an org, not by a
nested sub-org, and an admin key is already org-scoped. We therefore
treat that stored value as the OpenAI project ID to filter by
(`project_ids[]=<value>`) -- a deliberate, documented simplification: a
real deployment mapping multiple tenants to one OpenAI org would want a
`tenants.openai_project_id` column named accordingly, but for this
build's scope, reusing the one column keeps the schema and the code
example simple while remaining structurally accurate to the real API.
"""

from __future__ import annotations

from datetime import datetime

import httpx

from usage_providers.base import UsageProvider


class OpenAIUsageProvider(UsageProvider):
    def __init__(self, admin_key: str, base_url: str = "https://api.openai.com/v1", timeout_s: float = 30.0):
        self.admin_key = admin_key
        self.base_url = base_url.rstrip("/")
        self.timeout_s = timeout_s

    async def get_cost_usd(self, account_ref: str, window_start: datetime, window_end: datetime) -> float:
        total = 0.0
        page: str | None = None
        async with httpx.AsyncClient(timeout=self.timeout_s) as client:
            while True:
                params: dict = {
                    "start_time": int(window_start.timestamp()),
                    "end_time": int(window_end.timestamp()),
                    "bucket_width": "1d",
                }
                if account_ref:
                    params["project_ids[]"] = account_ref
                if page:
                    params["page"] = page

                resp = await client.get(
                    f"{self.base_url}/organization/costs",
                    headers={"Authorization": f"Bearer {self.admin_key}"},
                    params=params,
                )
                resp.raise_for_status()
                body = resp.json()

                for bucket in body.get("data", []):
                    for result in bucket.get("results", []):
                        total += float(result.get("amount", {}).get("value", 0.0))

                if not body.get("has_more"):
                    break
                page = body.get("next_page")
                if not page:
                    break

        return total
