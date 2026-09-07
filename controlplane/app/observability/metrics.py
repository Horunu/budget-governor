"""Prometheus metrics for the control plane. Unlike the gateway, this
service is not latency-critical, so metrics are wired via a plain ASGI
middleware rather than anything hand-optimized -- the point here is
visibility into the management API's own health (used for the "control
plane availability" panel in the Gateway Health dashboard), not proving
a latency budget.
"""

from __future__ import annotations

import time

from prometheus_client import CONTENT_TYPE_LATEST, Counter, Histogram, generate_latest
from starlette.middleware.base import BaseHTTPMiddleware
from starlette.requests import Request
from starlette.responses import Response

REQUESTS_TOTAL = Counter(
    "budget_governor_controlplane_requests_total",
    "Total control plane HTTP requests.",
    ["method", "path", "status_class"],
)

REQUEST_DURATION = Histogram(
    "budget_governor_controlplane_request_duration_seconds",
    "Control plane HTTP request latency.",
    ["method", "path"],
)


class MetricsMiddleware(BaseHTTPMiddleware):
    async def dispatch(self, request: Request, call_next):
        start = time.perf_counter()
        response = await call_next(request)
        elapsed = time.perf_counter() - start

        path = request.scope.get("route").path if request.scope.get("route") else request.url.path
        REQUESTS_TOTAL.labels(
            method=request.method, path=path, status_class=f"{response.status_code // 100}xx"
        ).inc()
        REQUEST_DURATION.labels(method=request.method, path=path).observe(elapsed)
        return response


def metrics_endpoint() -> Response:
    return Response(content=generate_latest(), media_type=CONTENT_TYPE_LATEST)
