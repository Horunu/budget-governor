"""Platform-level bootstrap auth: creating a brand new tenant is a
platform-operator action, not something a tenant's own admin-scoped API
key can do (a tenant admin key doesn't exist yet for a tenant that
doesn't exist yet, and letting ANY tenant's admin key provision new
tenants would let one customer create resources under another's
identity). Instead, POST /v1/tenants is gated by a shared platform token
set via the PLATFORM_BOOTSTRAP_TOKEN env var -- this is the credential an
operator (or scripts/seed.py) uses, not something issued per-tenant.
"""

from __future__ import annotations

import os

from fastapi import Header, HTTPException, status


def require_platform_token(x_platform_token: str | None = Header(default=None)) -> None:
    expected = os.environ.get("PLATFORM_BOOTSTRAP_TOKEN")
    if not expected:
        raise HTTPException(
            status_code=status.HTTP_503_SERVICE_UNAVAILABLE,
            detail={"message": "platform bootstrap token not configured", "reason": "not_configured"},
        )
    if not x_platform_token or x_platform_token != expected:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail={"message": "invalid or missing X-Platform-Token", "reason": "unauthenticated"},
        )
