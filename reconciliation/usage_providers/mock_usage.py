"""Deterministic, network-free stand-in for the real provider usage/cost
APIs -- what job.py uses when no provider admin keys are configured,
which is the default so `make smoke` demonstrates the full reconciliation
pipeline (including drift *detection*) without real credentials.

Rather than returning a value uncorrelated with reality (which would
make every run "drift" wildly and randomly), MockUsageProvider is
seeded with the gateway's OWN observed cost per account and applies a
small, deterministic (seeded by account_ref) perturbation to simulate
the kind of discrepancy a real provider's billing rounding, timing, or
estimation differences would produce. Most tenants land within a couple
percent (well under the default 5% threshold); one tenant is
deliberately seeded to drift further, so the demo's incidents list and
Grafana's Reconciliation Drift dashboard have something real to show.
"""

from __future__ import annotations

import hashlib
from datetime import datetime

from usage_providers.base import UsageProvider


class MockUsageProvider(UsageProvider):
    def __init__(self, reference_costs: dict[str, float], anomalous_account_ref: str | None = None):
        self.reference_costs = reference_costs
        # One account is deliberately pushed past the drift threshold so
        # the demo produces a real incident; picked by the caller (job.py
        # picks the highest-spend account so it's visually obvious in
        # the dashboard) rather than randomly, for reproducibility.
        self.anomalous_account_ref = anomalous_account_ref

    async def get_cost_usd(self, account_ref: str, window_start: datetime, window_end: datetime) -> float:
        reference = self.reference_costs.get(account_ref, 0.0)
        if reference == 0.0:
            return 0.0

        seed = hashlib.sha256(f"{account_ref}:{window_start.isoformat()}".encode()).hexdigest()
        # Map the first 8 hex chars to a stable float in [0, 1).
        fraction = int(seed[:8], 16) / 0xFFFFFFFF

        if account_ref == self.anomalous_account_ref:
            # 8-15% drift -- exceeds the default 5% threshold.
            pct = 0.08 + fraction * 0.07
        else:
            # 0-3% drift -- realistic billing-rounding noise, stays
            # comfortably under threshold.
            pct = fraction * 0.03

        # Provider-reported is usually slightly LOWER than gateway-
        # observed in practice (the gateway's pre-flight estimate errs
        # conservative; see internal/provider.EstimateTokens), so we bias
        # the perturbation in that direction rather than symmetric noise.
        return reference * (1 - pct)
