"""Append-only audit log for GDPR export/erase operations.

Owns `agent.data_audit_log` (a Postgres table in the `agent.*` schema) —
one row per served data-subject operation (`openspec/changes/agent-gdpr-compliance`,
Slice 4 of `AgentMemoryRoadmap.md`):

  - `record(user_id, action, summary)` — the only application insert.
    `payload_summary` carries COUNTS, never content (design.md D4): if it
    carried message text or preference values, the erase path would create
    a shadow copy of the very data it just deleted — a well-known
    compliance anti-pattern. Enforced by the signature
    (`dict[str, int]`); the endpoints pass the per-table counts they
    deleted/exported.
  - `retention_delete(days) -> int` — the only application delete, driven
    by the retention cron's audit iteration (span `agent.audit.retention`).
    Deletes rows older than the window and increments
    `agent_audit_retention_deleted_total` by the reaped count. The counter
    increments HERE (store-owned, like `sweeper.agent_runs_swept_total`) —
    callers must NOT increment it again or the metric double-counts.
  - `list_for_user(user_id)` — oldest-first read for tests and operator
    queries ("show me everything we did to this subject").

Append-only from application code: no UPDATE path exists, and
`retention_delete` is the sole DELETE. The table is evidence precisely
because nothing in the service can rewrite it (see alembic 0006).

Same retry posture as `ChatStore`: 100ms→200ms→400ms exponential backoff
for transient `asyncpg.PostgresConnectionError`; pool acquire timeout
raises `AuditLogUnavailable` immediately (5s default).

Defence in depth: every per-user read/delete carries the `user_id`
predicate; only the retention sweep is cross-user by design (it is keyed
on `requested_at`, the retention anchor, not on a subject).
"""
from __future__ import annotations

import asyncio
import json as _json
import logging
from dataclasses import dataclass
from datetime import datetime
from typing import Any

import asyncpg
from prometheus_client import Counter

log = logging.getLogger("nyx.agent.audit")

agent_audit_retention_deleted_total = Counter(
    "agent_audit_retention_deleted_total",
    "Total number of agent.data_audit_log rows deleted by the audit-retention cron.",
)


class AuditLogUnavailable(RuntimeError):
    """Raised when audit-log operations fail after the retry budget is exhausted."""


@dataclass(slots=True)
class AuditRow:
    """One audit row. `payload_summary` is the decoded JSON object (counts only)."""

    id: int
    user_id: int
    action: str
    payload_summary: dict[str, int]
    requested_at: datetime


class AuditLog:
    """Thin async wrapper over `agent.data_audit_log`.

    Connection pool owned by the caller (reused from the shared asyncpg
    pool — no second pool, same posture as `ChatStore`).
    """

    def __init__(
        self,
        pool: asyncpg.Pool,
        *,
        max_retries: int = 3,
        acquire_timeout: float = 5.0,
    ) -> None:
        self._pool = pool
        self._max_retries = max_retries
        self._acquire_timeout = acquire_timeout

    async def _with_retry(self, op_name: str, fn):
        """Run `fn(conn)` with retry/backoff for transient asyncpg errors.

        Identical semantics to `ChatStore._with_retry`: pool-acquire
        timeout raises `AuditLogUnavailable` immediately (retrying a
        saturated pool within the same request won't help); transient
        `asyncpg.PostgresConnectionError` retries with 100ms→200ms→400ms
        exponential backoff up to `max_retries`.
        """
        attempt = 0
        backoff_ms = 100
        while True:
            try:
                async with self._pool.acquire(timeout=self._acquire_timeout) as conn:
                    return await fn(conn)
            # Builtin `TimeoutError` — `asyncio.TimeoutError` is an alias of
            # it since 3.11 (same class, so the pool-acquire timeout is
            # caught identically; mirrors `PrefsStore._with_retry`).
            except TimeoutError as exc:
                log.warning(
                    "audit_log %s: pool acquire timeout after %.1fs (%r)",
                    op_name,
                    self._acquire_timeout,
                    exc,
                )
                raise AuditLogUnavailable(
                    f"{op_name} failed: pool acquire timeout "
                    f"({self._acquire_timeout}s)"
                ) from exc
            except asyncpg.PostgresConnectionError as exc:
                attempt += 1
                if attempt > self._max_retries:
                    log.warning(
                        "audit_log %s: retries exhausted (%r)", op_name, exc
                    )
                    raise AuditLogUnavailable(
                        f"{op_name} failed after {self._max_retries} retries"
                    ) from exc
                log.info(
                    "audit_log %s: retry %d/%d after %dms (%r)",
                    op_name,
                    attempt,
                    self._max_retries,
                    backoff_ms,
                    exc,
                )
                await asyncio.sleep(backoff_ms / 1000)
                backoff_ms *= 2

    async def record(
        self, user_id: int, action: str, summary: dict[str, int]
    ) -> None:
        """Append one audit row `(user_id, action, summary, now())`.

        The `dict[str, int]` signature IS the counts-not-content contract
        (design.md D4): values are per-table row counts (or timestamps as
        epoch ints), never message text, preference values, or any copy of
        the audited data — the audit must not become a shadow copy of what
        the erase path deletes. `record` is the only INSERT path; the
        table is append-only from application code (alembic 0006).

        The contract is enforced, not just declared (review L2,
        2026-10-14): every value must be a real `int` (bools rejected —
        they are ints in Python). A future caller passing `str(uuid)`
        would land an opaque identifier in the evidence table via the
        type annotation alone; now it raises instead.
        """
        if not all(
            isinstance(v, int) and not isinstance(v, bool)
            for v in summary.values()
        ):
            raise ValueError(
                "audit payload values must be int counts (counts-not-content "
                f"contract, design D4); got {summary!r}"
            )
        summary_json = _json.dumps(summary)

        async def _insert(conn: asyncpg.Connection) -> None:
            await conn.execute(
                "INSERT INTO data_audit_log "
                "(user_id, action, payload_summary) "
                "VALUES ($1, $2, $3::jsonb)",
                user_id,
                action,
                summary_json,
            )

        await self._with_retry("record", _insert)

    async def retention_delete(self, days: int) -> int:
        """Delete audit rows older than `days`. Returns the deleted count.

        The retention iteration's delete (design.md D5) — the only
        application DELETE on this table. `now() - ($1 * interval '1
        day')` is the canonical "N days ago" form in Postgres; works for
        any integer parameter without string-concatenation (which would
        force asyncpg to determine the literal type of `'$2 || days'` and
        fail with IndeterminateDatatypeError).

        Increments `agent_audit_retention_deleted_total` by the reaped
        count — the counter is store-owned so the metric cannot be
        forgotten by a caller. The retention cron must NOT increment it
        again (double-count).
        """

        async def _delete(conn: asyncpg.Connection) -> int:
            rows = await conn.fetch(
                "DELETE FROM data_audit_log "
                "WHERE requested_at < now() - ($1 * interval '1 day') "
                "RETURNING id",
                days,
            )
            return len(rows)

        deleted = await self._with_retry("retention_delete", _delete)
        agent_audit_retention_deleted_total.inc(deleted)
        return deleted

    async def list_for_user(self, user_id: int) -> list[AuditRow]:
        """Every audit row for one subject, oldest-first.

        For tests and operator queries ("what did we do, in order, and
        with what counts"). `payload_summary::text` + `json.loads`:
        asyncpg hands jsonb back as a raw string by default, and callers
        want the decoded object. The `id` tiebreaker keeps same-microsecond
        rows in insertion order.
        """

        async def _select(conn: asyncpg.Connection) -> list[AuditRow]:
            rows = await conn.fetch(
                "SELECT id, user_id, action, "
                "payload_summary::text AS payload_summary, requested_at "
                "FROM data_audit_log "
                "WHERE user_id = $1 "
                "ORDER BY requested_at ASC, id ASC",
                user_id,
            )
            return [_row_to_audit_row(r) for r in rows]

        return await self._with_retry("list_for_user", _select)


def _row_to_audit_row(row: asyncpg.Record | Any) -> AuditRow:
    return AuditRow(
        id=row["id"],
        user_id=row["user_id"],
        action=row["action"],
        payload_summary=_json.loads(row["payload_summary"]),
        requested_at=row["requested_at"],
    )
