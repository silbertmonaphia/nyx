"""Per-client-IP rate limit middleware for the Python agent.

Defense-in-depth against an LLM-cost-exhaustion attack from a
compromised container on the same bridge network as the agent.
The /agent/run hop has no app-layer auth — the network is the trust
boundary (see backend/agent_py/README.md "Trust model"). Per-IP rate
limiting is the in-process backstop for "what if a future container
on the internal network is compromised and floods us with /agent/run
calls".

The legitimate traffic pattern is: the Go backend (one container IP)
calls /agent/run at roughly the user chat throughput rate, well below
the default 100 RPS ceiling. A compromised container running at
>200 RPS would visibly distort Redis budget counters and burn LLM
tokens at a noticeable rate.

Algorithm: sliding window of request timestamps per IP. `burst` is
the number of requests allowed in any 1-second window. Lower
requests_per_second doesn't tighten this — `burst` is the ceiling.
The default `requests_per_second=100, burst=200` matches the Go chi
`middleware.DefaultRateLimit()` (10 req/s, burst 20) ×10 to account
for the fact that the chat backend fans out plan + step LLM calls
per request.
"""

from __future__ import annotations

import time
from collections import deque
from threading import Lock
from typing import Deque, Dict

from starlette.middleware.base import BaseHTTPMiddleware
from starlette.requests import Request
from starlette.responses import Response


class IpRateLimiter:
    """Sliding-window per-IP allow check."""

    def __init__(self, requests_per_second: float, burst: int):
        if requests_per_second <= 0:
            raise ValueError("requests_per_second must be positive")
        if burst <= 0:
            raise ValueError("burst must be positive")
        self.rps = requests_per_second
        self.burst = burst
        self.window_seconds = 1.0
        self._visitors: Dict[str, Deque[float]] = {}
        self._lock = Lock()

    def allow(self, client_ip: str) -> bool:
        now = time.monotonic()
        cutoff = now - self.window_seconds
        with self._lock:
            q = self._visitors.get(client_ip)
            if q is None:
                q = deque()
                self._visitors[client_ip] = q
            while q and q[0] < cutoff:
                q.popleft()
            if len(q) >= self.burst:
                return False
            q.append(now)
            return True

    def active_ips(self) -> int:
        """Number of IPs currently tracked. For tests + observability."""
        with self._lock:
            return len(self._visitors)


class RateLimitMiddleware(BaseHTTPMiddleware):
    """ASGI middleware that gates /agent/run by client IP. /healthz
    and /metrics stay open so compose + Prometheus can scrape without
    being rate-limited."""

    def __init__(self, app, limiter: IpRateLimiter):
        super().__init__(app)
        self.limiter = limiter

    async def dispatch(self, request: Request, call_next):
        if request.url.path != "/agent/run":
            return await call_next(request)
        # `request.client` is None in some ASGI test transports;
        # fall back to a synthetic key so test traffic isn't all
        # bucketed together.
        client_ip = request.client.host if request.client else "test"
        if not self.limiter.allow(client_ip):
            return Response(
                content='{"detail":"rate limit exceeded"}',
                status_code=429,
                media_type="application/json",
            )
        return await call_next(request)
