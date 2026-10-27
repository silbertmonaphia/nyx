"""ChatOpenAI construction with the BYOK SSRF guard attached.

Every planner/executor/prefs/title client is built here. The `ctor`
argument is the caller module's own `ChatOpenAI` symbol (not imported
here) so the existing per-module test seams keep working: tests patch
`agent.planner.ChatOpenAI` and friends, and that patched symbol is what
the factory calls.

A user-supplied endpoint (BYOK) gets an `httpx.AsyncClient` whose request
hook classifies the target host before every send (redirect hops
included); deployment endpoints are left untouched, matching the Go
side's operator-trust posture. The hook path is used instead of a
custom transport because `httpx.AsyncClient` only mounts the
environment proxies when `transport is None` and `trust_env` is set —
a custom transport would silently bypass `HTTP(S)_PROXY` on proxy-only
deployments. `PYTHON_AGENT_BYOK_ALLOW_PRIVATE=true` lifts the guard for
single-operator instances (e.g. a loopback vLLM in the form).
"""

from __future__ import annotations

from typing import Any

import httpx

from .config import Settings
from .net_guard import Resolver, ensure_public_address

_guarded_client: httpx.AsyncClient | None = None


def guarded_async_client(
    *, allow_private: bool = False, resolver: Resolver | None = None
) -> httpx.AsyncClient:
    async def _guard(request: httpx.Request) -> None:
        if not allow_private:
            await ensure_public_address(str(request.url), resolver=resolver)

    return httpx.AsyncClient(
        follow_redirects=True,
        event_hooks={"request": [_guard]},
    )


def _default_guarded_client() -> httpx.AsyncClient:
    global _guarded_client
    if _guarded_client is None:
        _guarded_client = guarded_async_client()
    return _guarded_client


def build_chat_model(
    ctor: Any,
    *,
    base_url: str,
    api_key: str | Any,
    model: str,
    user_supplied: bool,
    settings: Settings,
    **kwargs: Any,
) -> Any:
    if (
        user_supplied
        and not settings.byok_allow_private
        and "http_async_client" not in kwargs
    ):
        kwargs["http_async_client"] = _default_guarded_client()
    return ctor(base_url=base_url, api_key=api_key, model=model, **kwargs)
