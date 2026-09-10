"""FastAPI auth dependencies: resolve an API key to a tenant + scopes, and
enforce that a required scope is present. Every mutating endpoint requires
`admin`; read endpoints accept `admin`, `agent` (scoped to its own data --
enforced per-route, not here), or `viewer`.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime, timezone

from fastapi import Depends, Header, HTTPException, status
from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from app.auth.keys import key_prefix, verify_key
from app.db.database import get_session
from app.db.models import ApiKey


@dataclass
class Identity:
    tenant_id: str
    key_id: str
    scopes: set[str] = field(default_factory=set)
    agent_id: str | None = None

    def has_scope(self, *scopes: str) -> bool:
        return bool(self.scopes.intersection(scopes))

    def is_agent_only(self) -> bool:
        """True for a key that carries ONLY the agent scope (no admin/
        viewer) -- used to restrict spend queries to that one agent's
        own data, per the "agent scope can read its own data only" rule.
        """
        return self.scopes == {"agent"}


async def get_current_identity(
    authorization: str | None = Header(default=None),
    session: AsyncSession = Depends(get_session),
) -> Identity:
    if not authorization or not authorization.startswith("Bearer "):
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail={"message": "missing or malformed Authorization header", "reason": "unauthenticated"},
        )
    raw_key = authorization.removeprefix("Bearer ").strip()
    if len(raw_key) < 12:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail={"message": "invalid api key", "reason": "unauthenticated"},
        )

    prefix = key_prefix(raw_key)
    result = await session.execute(
        select(ApiKey).where(ApiKey.key_prefix == prefix, ApiKey.revoked_at.is_(None))
    )
    for row in result.scalars():
        if verify_key(raw_key, row.hashed_key):
            return Identity(
                tenant_id=row.tenant_id,
                key_id=row.id,
                scopes=set(row.scopes),
                agent_id=row.agent_id,
            )

    raise HTTPException(
        status_code=status.HTTP_401_UNAUTHORIZED,
        detail={"message": "invalid api key", "reason": "unauthenticated"},
    )


def require_scopes(*scopes: str):
    """Dependency factory: raises 403 unless the resolved identity has at
    least one of `scopes`. Use as `Depends(require_scopes("admin"))`.
    """

    async def checker(identity: Identity = Depends(get_current_identity)) -> Identity:
        if not identity.has_scope(*scopes):
            raise HTTPException(
                status_code=status.HTTP_403_FORBIDDEN,
                detail={"message": f"requires one of scopes: {scopes}", "reason": "insufficient_scope"},
            )
        return identity

    return checker
