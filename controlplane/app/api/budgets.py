from __future__ import annotations

from datetime import datetime, timezone

from fastapi import APIRouter, Depends, HTTPException, status
from pydantic import BaseModel
from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from app.auth.dependencies import Identity, get_current_identity, require_scopes
from app.db.database import get_session
from app.db.models import Budget

router = APIRouter(prefix="/v1/budgets", tags=["budgets"])


class BudgetResponse(BaseModel):
    id: str
    tenant_id: str
    agent_id: str | None
    task_id: str | None
    token_limit: int | None
    dollar_limit: float | None
    refill_rate_per_min: float
    period_seconds: int
    updated_at: datetime

    model_config = {"from_attributes": True}


class UpsertBudgetRequest(BaseModel):
    agent_id: str | None = None
    task_id: str | None = None
    token_limit: int | None = None
    dollar_limit: float | None = None
    refill_rate_per_min: float = 0
    period_seconds: int = 86400


def _require_own_tenant(identity: Identity, tenant_id: str) -> None:
    if identity.tenant_id != tenant_id:
        raise HTTPException(
            status_code=status.HTTP_403_FORBIDDEN,
            detail={"message": "cannot access another tenant's budgets", "reason": "insufficient_scope"},
        )


@router.get("/{tenant_id}", response_model=list[BudgetResponse])
async def get_budgets(
    tenant_id: str,
    identity: Identity = Depends(get_current_identity),
    session: AsyncSession = Depends(get_session),
) -> list[Budget]:
    _require_own_tenant(identity, tenant_id)

    query = select(Budget).where(Budget.tenant_id == tenant_id)
    if identity.is_agent_only() and identity.agent_id:
        # An agent-scoped key sees its own agent-level/task-level budgets
        # plus the tenant-level ceiling (it's still subject to it), but
        # not other agents' budgets.
        query = query.where((Budget.agent_id == identity.agent_id) | (Budget.agent_id.is_(None)))
    result = await session.execute(query)
    return list(result.scalars())


@router.post("/{tenant_id}", response_model=BudgetResponse, status_code=status.HTTP_200_OK)
async def upsert_budget(
    tenant_id: str,
    body: UpsertBudgetRequest,
    identity: Identity = Depends(require_scopes("admin")),
    session: AsyncSession = Depends(get_session),
) -> Budget:
    _require_own_tenant(identity, tenant_id)

    if body.token_limit is None and body.dollar_limit is None:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={"message": "at least one of token_limit or dollar_limit must be set", "reason": "invalid_request"},
        )

    result = await session.execute(
        select(Budget).where(
            Budget.tenant_id == tenant_id,
            Budget.agent_id == body.agent_id,
            Budget.task_id == body.task_id,
        )
    )
    budget = result.scalar_one_or_none()
    if budget is None:
        budget = Budget(tenant_id=tenant_id, agent_id=body.agent_id, task_id=body.task_id)
        session.add(budget)

    budget.token_limit = body.token_limit
    budget.dollar_limit = body.dollar_limit
    budget.refill_rate_per_min = body.refill_rate_per_min
    budget.period_seconds = body.period_seconds
    budget.updated_at = datetime.now(timezone.utc)

    await session.commit()
    await session.refresh(budget)
    return budget
