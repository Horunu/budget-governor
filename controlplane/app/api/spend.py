from __future__ import annotations

import re
from datetime import datetime, timedelta, timezone

from fastapi import APIRouter, Depends, HTTPException, Query, status
from pydantic import BaseModel
from sqlalchemy import func, select
from sqlalchemy.ext.asyncio import AsyncSession

from app.auth.dependencies import Identity, get_current_identity
from app.db.database import get_session
from app.db.models import SpendEvent

router = APIRouter(prefix="/v1/spend", tags=["spend"])

_WINDOW_RE = re.compile(r"^(\d+)(h|d)$")


def parse_window(window: str) -> timedelta:
    m = _WINDOW_RE.match(window)
    if not m:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={"message": f"invalid window {window!r}, expected e.g. 1h, 24h, 7d", "reason": "invalid_request"},
        )
    n, unit = int(m.group(1)), m.group(2)
    return timedelta(hours=n) if unit == "h" else timedelta(days=n)


class ModelBreakdown(BaseModel):
    model: str
    tokens_in: int
    tokens_out: int
    cost_usd: float
    request_count: int


class AgentBreakdown(BaseModel):
    agent_id: str | None
    tokens_in: int
    tokens_out: int
    cost_usd: float
    request_count: int


class SpendSummary(BaseModel):
    tenant_id: str
    window: str
    window_start: datetime
    total_tokens_in: int
    total_tokens_out: int
    total_cost_usd: float
    total_requests: int
    by_model: list[ModelBreakdown]
    by_agent: list[AgentBreakdown]


@router.get("/{tenant_id}", response_model=SpendSummary)
async def get_spend(
    tenant_id: str,
    window: str = Query(default="24h"),
    identity: Identity = Depends(get_current_identity),
    session: AsyncSession = Depends(get_session),
) -> SpendSummary:
    if identity.tenant_id != tenant_id:
        raise HTTPException(
            status_code=status.HTTP_403_FORBIDDEN,
            detail={"message": "cannot access another tenant's spend", "reason": "insufficient_scope"},
        )

    delta = parse_window(window)
    window_start = datetime.now(timezone.utc) - delta

    base_filters = [SpendEvent.tenant_id == tenant_id, SpendEvent.created_at >= window_start]
    if identity.is_agent_only() and identity.agent_id:
        # "Agent scope can read its own data only."
        base_filters.append(SpendEvent.agent_id == identity.agent_id)

    totals_row = (
        await session.execute(
            select(
                func.coalesce(func.sum(SpendEvent.tokens_in), 0),
                func.coalesce(func.sum(SpendEvent.tokens_out), 0),
                func.coalesce(func.sum(SpendEvent.cost_usd), 0),
                func.count(SpendEvent.id),
            ).where(*base_filters)
        )
    ).one()
    total_tokens_in, total_tokens_out, total_cost_usd, total_requests = totals_row

    by_model_rows = await session.execute(
        select(
            SpendEvent.model,
            func.coalesce(func.sum(SpendEvent.tokens_in), 0),
            func.coalesce(func.sum(SpendEvent.tokens_out), 0),
            func.coalesce(func.sum(SpendEvent.cost_usd), 0),
            func.count(SpendEvent.id),
        )
        .where(*base_filters)
        .group_by(SpendEvent.model)
        .order_by(func.sum(SpendEvent.cost_usd).desc())
    )
    by_model = [
        ModelBreakdown(model=m, tokens_in=ti, tokens_out=to, cost_usd=float(c), request_count=rc)
        for m, ti, to, c, rc in by_model_rows
    ]

    by_agent_rows = await session.execute(
        select(
            SpendEvent.agent_id,
            func.coalesce(func.sum(SpendEvent.tokens_in), 0),
            func.coalesce(func.sum(SpendEvent.tokens_out), 0),
            func.coalesce(func.sum(SpendEvent.cost_usd), 0),
            func.count(SpendEvent.id),
        )
        .where(*base_filters)
        .group_by(SpendEvent.agent_id)
        .order_by(func.sum(SpendEvent.cost_usd).desc())
    )
    by_agent = [
        AgentBreakdown(agent_id=a, tokens_in=ti, tokens_out=to, cost_usd=float(c), request_count=rc)
        for a, ti, to, c, rc in by_agent_rows
    ]

    return SpendSummary(
        tenant_id=tenant_id,
        window=window,
        window_start=window_start,
        total_tokens_in=total_tokens_in,
        total_tokens_out=total_tokens_out,
        total_cost_usd=float(total_cost_usd),
        total_requests=total_requests,
        by_model=by_model,
        by_agent=by_agent,
    )
