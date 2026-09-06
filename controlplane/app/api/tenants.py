from __future__ import annotations

from fastapi import APIRouter, Depends, HTTPException, Request, status
from pydantic import BaseModel, Field
from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from app.auth.dependencies import Identity, get_current_identity
from app.auth.keys import generate_api_key, hash_key, key_prefix
from app.auth.platform import require_platform_token
from app.db.database import get_session
from app.db.models import ApiKey, Tenant

router = APIRouter(prefix="/v1/tenants", tags=["tenants"])


class CreateTenantRequest(BaseModel):
    name: str = Field(min_length=1, max_length=200)
    openai_org_id: str | None = None
    anthropic_workspace_id: str | None = None


class TenantResponse(BaseModel):
    id: str
    name: str

    model_config = {"from_attributes": True}


class IssueApiKeyRequest(BaseModel):
    scopes: list[str] = Field(default_factory=lambda: ["admin"])
    label: str | None = None


class IssuedApiKeyResponse(BaseModel):
    id: str
    tenant_id: str
    scopes: list[str]
    # Shown exactly once, at issuance -- only the bcrypt hash is
    # persisted. If a client loses this value, the only recovery path is
    # revoking and issuing a new key.
    api_key: str


@router.post("", response_model=TenantResponse, status_code=status.HTTP_201_CREATED, dependencies=[Depends(require_platform_token)])
async def create_tenant(body: CreateTenantRequest, session: AsyncSession = Depends(get_session)) -> Tenant:
    existing = await session.execute(select(Tenant).where(Tenant.name == body.name))
    if existing.scalar_one_or_none() is not None:
        raise HTTPException(status_code=status.HTTP_409_CONFLICT, detail={"message": "tenant name already exists", "reason": "conflict"})

    tenant = Tenant(
        name=body.name,
        openai_org_id=body.openai_org_id,
        anthropic_workspace_id=body.anthropic_workspace_id,
    )
    session.add(tenant)
    await session.commit()
    await session.refresh(tenant)
    return tenant


async def _tenant_or_404(session: AsyncSession, tenant_id: str) -> Tenant:
    tenant = await session.get(Tenant, tenant_id)
    if tenant is None:
        raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail={"message": "tenant not found", "reason": "not_found"})
    return tenant


async def _authorize_key_issuance(request: Request, tenant_id: str, session: AsyncSession) -> None:
    """Key issuance for an existing tenant is allowed either via the
    platform bootstrap token, or via an existing admin-scoped key that
    already belongs to this same tenant (key rotation / adding an
    additional scoped key without needing the platform token every time).
    """
    platform_token = request.headers.get("x-platform-token") or request.headers.get("X-Platform-Token")
    import os

    expected = os.environ.get("PLATFORM_BOOTSTRAP_TOKEN")
    if expected and platform_token == expected:
        return

    authorization = request.headers.get("authorization") or request.headers.get("Authorization")
    if authorization and authorization.startswith("Bearer "):
        try:
            identity: Identity = await get_current_identity(authorization=authorization, session=session)
        except HTTPException:
            identity = None  # fall through to the generic 401 below
        if identity is not None and identity.tenant_id == tenant_id and identity.has_scope("admin"):
            return

    raise HTTPException(
        status_code=status.HTTP_401_UNAUTHORIZED,
        detail={"message": "requires platform token or an admin key for this tenant", "reason": "unauthenticated"},
    )


@router.post("/{tenant_id}/api-keys", response_model=IssuedApiKeyResponse, status_code=status.HTTP_201_CREATED)
async def issue_api_key(
    tenant_id: str,
    body: IssueApiKeyRequest,
    request: Request,
    session: AsyncSession = Depends(get_session),
) -> IssuedApiKeyResponse:
    await _tenant_or_404(session, tenant_id)
    await _authorize_key_issuance(request, tenant_id, session)

    valid_scopes = {"admin", "agent", "viewer"}
    if not set(body.scopes).issubset(valid_scopes) or not body.scopes:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={"message": f"scopes must be a non-empty subset of {sorted(valid_scopes)}", "reason": "invalid_request"},
        )

    raw_key = generate_api_key()
    record = ApiKey(
        tenant_id=tenant_id,
        key_prefix=key_prefix(raw_key),
        hashed_key=hash_key(raw_key),
        scopes=body.scopes,
        label=body.label,
    )
    session.add(record)
    await session.commit()
    await session.refresh(record)

    return IssuedApiKeyResponse(id=record.id, tenant_id=tenant_id, scopes=body.scopes, api_key=raw_key)
