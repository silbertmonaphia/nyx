"""Inbound service-token gate for the agent's HTTP surface.

The Go backend is the only intended caller. Every request except the
exact `/healthz` probe path must carry `X-Nyx-Service-Token` matching
`AGENT_SERVICE_TOKEN`; the comparison is constant-time and operates on
bytes so any header value (including non-ASCII input) is rejected with
401 instead of raising.

The middleware is pure ASGI: it never buffers or rewrites a streaming
response, and it runs before any handler or rate-limit state.
"""

from __future__ import annotations

import hmac

from starlette.responses import Response
from starlette.types import ASGIApp, Receive, Scope, Send

from .config import MIN_SERVICE_TOKEN_BYTES

SERVICE_TOKEN_HEADER = "x-nyx-service-token"  # noqa: S105
HEADER_BYTES = SERVICE_TOKEN_HEADER.encode("ascii")
HEALTH_PATH = "/healthz"

_UNAUTHORIZED_BODY = b'{"detail":"unauthorized"}'


class ServiceTokenMiddleware:
    def __init__(self, app: ASGIApp, expected_token: str) -> None:
        expected = expected_token.encode("utf-8")
        if len(expected) < MIN_SERVICE_TOKEN_BYTES:
            raise ValueError(
                f"AGENT_SERVICE_TOKEN must be at least {MIN_SERVICE_TOKEN_BYTES} bytes"
            )
        self.app = app
        self._expected = expected

    async def __call__(self, scope: Scope, receive: Receive, send: Send) -> None:
        if scope["type"] == "lifespan":
            await self.app(scope, receive, send)
            return
        if scope["type"] != "http":
            return
        if scope["path"] == HEALTH_PATH:
            await self.app(scope, receive, send)
            return

        provided = b""
        for name, value in scope.get("headers", []):
            if name == HEADER_BYTES:
                provided = value
                break

        if not hmac.compare_digest(provided, self._expected):
            response = Response(
                content=_UNAUTHORIZED_BODY,
                status_code=401,
                media_type="application/json",
            )
            await response(scope, receive, send)
            return

        await self.app(scope, receive, send)
