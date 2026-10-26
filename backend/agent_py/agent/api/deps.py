"""Shared FastAPI dependencies for the agent's `api` package.

Extracted during the GDPR review (N4, 2026-10-14): the
`X-Nyx-User-Id` reader existed byte-identically in three places
(`api/chat_sessions.py`, `api/gdpr.py`, and inline in `main.py`'s run
handler). One copy keeps the 400 strings and the loud-failure semantics
from drifting.
"""

from __future__ import annotations

from fastapi import HTTPException, Request


def require_user_id(request: Request) -> int:
    """Read the Go-verified user id from `X-Nyx-User-Id`.

    The service-token middleware has already authenticated the caller, so
    this is NOT an auth check: it is a loud 400 for a misconfigured caller,
    which would otherwise silently read or write user 0's data.
    """
    user_id_raw = request.headers.get("x-nyx-user-id", "").strip()
    if not user_id_raw:
        raise HTTPException(
            status_code=400,
            detail="missing X-Nyx-User-Id header (set by the Go chat handler)",
        )
    try:
        return int(user_id_raw)
    except ValueError as exc:
        raise HTTPException(
            status_code=400,
            detail=f"X-Nyx-User-Id must be an integer, got {user_id_raw!r}",
        ) from exc
