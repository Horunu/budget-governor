"""API key generation and bcrypt hashing.

Uses the `bcrypt` package directly (not passlib) so the hash format
($2b$...) is byte-for-byte interoperable with the Go gateway's
golang.org/x/crypto/bcrypt verification in gateway/internal/auth --
both implement the same standard bcrypt algorithm, so a key issued here
authenticates identically against either service.
"""

from __future__ import annotations

import secrets
import string

import bcrypt

_ALPHABET = string.ascii_letters + string.digits
_KEY_LENGTH = 32


def generate_api_key(prefix: str = "bg_live") -> str:
    """Generates a new raw API key. The first 12 characters (including
    the prefix) are later stored in plaintext as `key_prefix` for fast
    lookup -- see docs/ARCHITECTURE.md's auth section.
    """
    body = "".join(secrets.choice(_ALPHABET) for _ in range(_KEY_LENGTH))
    return f"{prefix}_{body}"


def hash_key(raw_key: str) -> str:
    return bcrypt.hashpw(raw_key.encode("utf-8"), bcrypt.gensalt()).decode("utf-8")


def verify_key(raw_key: str, hashed_key: str) -> bool:
    try:
        return bcrypt.checkpw(raw_key.encode("utf-8"), hashed_key.encode("utf-8"))
    except ValueError:
        # Malformed stored hash -- never treat as a match.
        return False


def key_prefix(raw_key: str, length: int = 12) -> str:
    return raw_key[:length]
