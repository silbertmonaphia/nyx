"""Per-user hourly token budget guard.

Redis-backed counter at `agent:budget:u={user_id}:h={utc_hour}` with TTL 90 min on first
increment. Consulted before each `LLM.chat` call inside the loop. Fails closed on any
Redis error (`decision="unavailable"`); never raises.

OTel span `agent.budget` wraps every check with attributes:
  - user_id (int)
  - tokens_used (int — value after `INCRBY`)
  - tokens_remaining (int — `cap - tokens_used`, may be negative)
  - decision (`allow` | `exceeded` | `unavailable`)
"""

from __future__ import annotations

import logging
from datetime import datetime, timezone

from opentelemetry import trace
from redis.asyncio import Redis

from .types import BudgetDecision

log = logging.getLogger("nyx.agent.budget")

# 90 minutes — deliberately longer than the one-hour budget window, but the
# TTL is NOT what enforces the window. The window lives in the KEY
# (`h={utc_hour}`, recomputed on every check): the next hour computes a
# different key, so no read ever touches an expired hour's counter. The TTL
# exists only so Redis garbage-collects old counters; the 30-minute headroom
# over the window is margin for clock skew between the app (which derives the
# key) and Redis (which runs the TTL) plus last-second touches. `EXPIRE` rolls
# on every check (not NX), refreshing the countdown.
_TTL_SECONDS = 90 * 60


def utc_hour_key(user_id: int, now: datetime | None = None) -> str:
    """`agent:budget:u={user_id}:h={YYYYMMDDHH}` in UTC."""
    when = now or datetime.now(timezone.utc)
    return f"agent:budget:u={user_id}:h={when.strftime('%Y%m%d%H')}"


class BudgetGuard:
    def __init__(self, redis: Redis, cap: int) -> None:
        self._redis = redis
        self._cap = cap

    @property
    def cap(self) -> int:
        return self._cap

    async def check_and_increment(
        self,
        user_id: int,
        estimated_tokens: int,
        tracer: trace.Tracer,
    ) -> BudgetDecision:
        key = utc_hour_key(user_id)
        with tracer.start_as_current_span("agent.budget") as span:
            span.set_attribute("user_id", user_id)
            try:
                # INCRBY returns the new value. If the key didn't exist, Redis creates it
                # at 0 then adds. We then set EXPIRE (NX would skip if already TTL'd; we
                # use plain EXPIRE since rolling the TTL is fine — the key only matters
                # within the hour it was created).
                new_value = await self._redis.incrby(key, estimated_tokens)
                await self._redis.expire(key, _TTL_SECONDS)
            except Exception as exc:  # noqa: BLE001 — we deliberately swallow all
                span.add_event("redis_error", {"error": repr(exc)})
                span.set_attribute("decision", "unavailable")
                # Make the failure diagnosable: the wire only carries
                # `budget_unavailable`, which can't distinguish "wrong
                # host/port" from "Redis is down". Log the target host:port
                # (never credentials) so the agent log names it. Hint: the
                # host-mapped Redis port for this project is 6380, not 6379.
                try:
                    kw = self._redis.connection_pool.connection_kwargs
                    target = f"{kw.get('host', '?')}:{kw.get('port', '?')}"
                except Exception:  # noqa: BLE001 — stub Redis objects in tests
                    target = "?"
                log.warning("budget guard: redis unreachable at %s: %r", target, exc)
                return BudgetDecision(
                    decision="unavailable",
                    tokens_used=0,
                    tokens_remaining=self._cap,
                )

            tokens_used = int(new_value)

            # MEDIUM-10: refused calls don't count toward the budget. Roll the
            # increment back so the user's hourly counter only includes calls we
            # actually let through. Race-safe: the (INCRBY + DECRBY) pair runs
            # against a single key in the same Redis instance; concurrent calls
            # may transiently push the counter above cap, but the final value is
            # the sum of *allowed* calls only.
            if tokens_used > self._cap:
                try:
                    await self._redis.decrby(key, estimated_tokens)
                except Exception:  # noqa: BLE001 — rollback is best-effort
                    pass
                tokens_used -= estimated_tokens  # local view; the real counter may differ
                tokens_remaining = self._cap - tokens_used
                decision = "exceeded"
            else:
                tokens_remaining = self._cap - tokens_used
                decision = "allow"

            span.set_attribute("tokens_used", max(tokens_used, 0))
            span.set_attribute("tokens_remaining", max(tokens_remaining, 0))
            span.set_attribute("decision", decision)
            return BudgetDecision(
                decision=decision,  # type: ignore[arg-type]
                tokens_used=max(tokens_used, 0),
                tokens_remaining=max(tokens_remaining, 0),
            )