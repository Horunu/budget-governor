from __future__ import annotations

from fastapi import APIRouter, Depends, HTTPException, status
from pydantic import BaseModel, Field
from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from app.auth.dependencies import Identity, require_scopes
from app.auth.keys import generate_api_key, hash_key, key_prefix
from app.db.database import get_session
from app.db.models import Agent, ApiKey

router = APIRouter(prefix="/v1/agents", tags=["agents"])


class CreateAgentRequest(BaseModel):
    name: str = Field(min_length=1, max_length=200)


class CreateAgentResponse(BaseModel):
    id: str
    tenant_id: str
    name: str
    api_key: str  # scope=["agent"], shown once


@router.post("", response_model=CreateAgentResponse, status_code=status.HTTP_201_CREATED)
async def create_agent(
    body: CreateAgentRequest,
    identity: Identity = Depends(require_scopes("admin")),
    session: AsyncSession = Depends(get_session),
) -> CreateAgentResponse:
    existing = await session.execute(
        select(Agent).where(Agent.tenant_id == identity.tenant_id, Agent.name == body.name)
    )
    if existing.scalar_one_or_none() is not None:
        raise HTTPException(
            status_code=status.HTTP_409_CONFLICT,
            detail={"message": "an agent with this name already exists for this tenant", "reason": "conflict"},
        )

    agent = Agent(tenant_id=identity.tenant_id, name=body.name)
    session.add(agent)
    await session.flush()  # populate agent.id before using it below

    raw_key = generate_api_key()
    key_record = ApiKey(
        tenant_id=identity.tenant_id,
        agent_id=agent.id,
        key_prefix=key_prefix(raw_key),
        hashed_key=hash_key(raw_key),
        scopes=["agent"],
        label=f"agent:{agent.name}",
    )
    session.add(key_record)
    await session.commit()
    await session.refresh(agent)

    return CreateAgentResponse(id=agent.id, tenant_id=agent.tenant_id, name=agent.name, api_key=raw_key)
