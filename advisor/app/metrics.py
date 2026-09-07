"""Prometheus metrics for the advisor -- primarily invocation counts and
latency, since (per ADR-003) this service is never on the gateway's hot
path and its own latency only matters for the one throttled request that
triggered it.
"""

from __future__ import annotations

import time

from prometheus_client import CONTENT_TYPE_LATEST, Counter, Histogram, generate_latest
from starlette.middleware.base import BaseHTTPMiddleware
from starlette.requests import Request
from starlette.responses import Response

ADVISE_REQUESTS_TOTAL = Counter(
    "budget_governor_advisor_requests_total",
    "Total /v1/advise calls, by outcome (suggested/no_suggestions/error).",
    ["outcome"],
)

ADVISE_DURATION = Histogram(
    "budget_governor_advisor_duration_seconds",
    "Advisor request latency (including the LLM call).",
)


class MetricsMiddleware(BaseHTTPMiddleware):
    async def dispatch(self, request: Request, call_next):
        if request.url.path != "/v1/advise":
            return await call_next(request)

        start = time.perf_counter()
        response = await call_next(request)
        ADVISE_DURATION.observe(time.perf_counter() - start)
        return response


def metrics_endpoint() -> Response:
    return Response(content=generate_latest(), media_type=CONTENT_TYPE_LATEST)
