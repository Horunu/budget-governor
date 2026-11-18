from __future__ import annotations

from datetime import datetime, timezone

from fastapi import APIRouter, Depends, HTTPException, status
from pydantic import BaseModel, Field
from sqlalchemy import select, update
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


class AgentResponse(BaseModel):
    id: str
    name: str
    active: bool  # False once every key for this agent is revoked



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

    raw_key = _add_agent_key(session, agent)
    await session.commit()
    await session.refresh(agent)

    return CreateAgentResponse(id=agent.id, tenant_id=agent.tenant_id, name=agent.name, api_key=raw_key)


def _add_agent_key(session: AsyncSession, agent: Agent) -> str:
    raw_key = generate_api_key()
    session.add(
        ApiKey(
            tenant_id=agent.tenant_id,
            agent_id=agent.id,
            key_prefix=key_prefix(raw_key),
            hashed_key=hash_key(raw_key),
            scopes=["agent"],
            label=f"agent:{agent.name}",
        )
    )
    return raw_key


async def _revoke_agent_keys(session: AsyncSession, agent: Agent) -> None:
    await session.execute(
        update(ApiKey)
        .where(ApiKey.agent_id == agent.id, ApiKey.revoked_at.is_(None))
        .values(revoked_at=datetime.now(timezone.utc))
    )


async def _own_agent_or_404(session: AsyncSession, identity: Identity, agent_id: str) -> Agent:
    agent = await session.get(Agent, agent_id)
    if agent is None or agent.tenant_id != identity.tenant_id:
        raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail={"message": "agent not found", "reason": "not_found"})
    return agent


@router.get("", response_model=list[AgentResponse])
async def list_agents(
    identity: Identity = Depends(require_scopes("admin", "viewer")),
    session: AsyncSession = Depends(get_session),
) -> list[AgentResponse]:
    agents = (await session.execute(select(Agent).where(Agent.tenant_id == identity.tenant_id).order_by(Agent.name))).scalars().all()
    active_ids = set(
        (
            await session.execute(
                select(ApiKey.agent_id).where(ApiKey.tenant_id == identity.tenant_id, ApiKey.revoked_at.is_(None))
            )
        ).scalars()
    )
    return [AgentResponse(id=a.id, name=a.name, active=a.id in active_ids) for a in agents]


@router.post("/{agent_id}/rotate-key", response_model=CreateAgentResponse)
async def rotate_agent_key(
    agent_id: str,
    identity: Identity = Depends(require_scopes("admin")),
    session: AsyncSession = Depends(get_session),
) -> CreateAgentResponse:
    """Revokes the agent's current key(s) and issues a new one (lost key, laptop change)."""
    agent = await _own_agent_or_404(session, identity, agent_id)
    await _revoke_agent_keys(session, agent)
    raw_key = _add_agent_key(session, agent)
    await session.commit()
    return CreateAgentResponse(id=agent.id, tenant_id=agent.tenant_id, name=agent.name, api_key=raw_key)


@router.delete("/{agent_id}", status_code=status.HTTP_204_NO_CONTENT)
async def remove_agent(
    agent_id: str,
    identity: Identity = Depends(require_scopes("admin")),
    session: AsyncSession = Depends(get_session),
) -> None:
    """Revokes every key for the agent. The agent row and its spend history stay."""
    agent = await _own_agent_or_404(session, identity, agent_id)
    await _revoke_agent_keys(session, agent)
    await session.commit()
