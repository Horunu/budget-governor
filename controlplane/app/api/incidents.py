from __future__ import annotations

from datetime import datetime

from fastapi import APIRouter, Depends, Query
from pydantic import BaseModel
from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from app.auth.dependencies import Identity, get_current_identity
from app.db.database import get_session
from app.db.models import ReconciliationDrift

router = APIRouter(prefix="/v1/incidents", tags=["incidents"])


class IncidentResponse(BaseModel):
    id: int
    run_id: int
    tenant_id: str
    gateway_observed_usd: float
    provider_reported_usd: float
    drift_amount_usd: float
    drift_pct: float
    resolved_at: datetime | None
    created_at: datetime

    model_config = {"from_attributes": True}


@router.get("", response_model=list[IncidentResponse])
async def list_incidents(
    unresolved: bool = Query(default=True),
    identity: Identity = Depends(get_current_identity),
    session: AsyncSession = Depends(get_session),
) -> list[ReconciliationDrift]:
    # Incidents are scoped to the caller's own tenant -- there is no
    # cross-tenant view in this API, consistent with every other
    # tenant-scoped endpoint.
    query = select(ReconciliationDrift).where(ReconciliationDrift.tenant_id == identity.tenant_id)
    if unresolved:
        query = query.where(ReconciliationDrift.resolved_at.is_(None))
    query = query.order_by(ReconciliationDrift.created_at.desc())

    result = await session.execute(query)
    return list(result.scalars())
