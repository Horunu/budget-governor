"""Budget Governor control plane: tenant/agent/API-key management, budget
CRUD, spend queries, and reconciliation incident listing. See
docs/ARCHITECTURE.md for how this fits alongside the gateway, advisor,
and reconciliation job.
"""

from __future__ import annotations

from contextlib import asynccontextmanager

from fastapi import FastAPI

from app.api import agents, budgets, health, incidents, spend, tenants
from app.db import database
from app.observability.metrics import MetricsMiddleware, metrics_endpoint


@asynccontextmanager
async def lifespan(app: FastAPI):
    database.configure()
    yield
    await database.dispose()


app = FastAPI(
    title="Budget Governor Control Plane",
    version="0.1.0",
    lifespan=lifespan,
)

app.add_middleware(MetricsMiddleware)
app.add_route("/metrics", lambda request: metrics_endpoint(), methods=["GET"])

app.include_router(health.router)
app.include_router(tenants.router)
app.include_router(agents.router)
app.include_router(budgets.router)
app.include_router(spend.router)
app.include_router(incidents.router)
