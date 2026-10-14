"""Thin metadata layer for agent runs.

Owns `agent.runs` (a Postgres table in the `agent.*` schema) for user-facing
queries that langGraph's opaque `checkpoint_*` blob doesn't expose:

  - `list_runs_for_user(user_id, limit, offset)` — paginated history for the
    "show me my runs" UX.
  - `mark_completed` / `mark_failed` — terminal state transitions.
  - `create_run(user_id) -> uuid` — mints a server-side uuid4 for fresh runs.
  - `get_run(run_id, user_id)` — defensive per-user read used by the resume
    decision in `main.py`.
  - `touch_heartbeat(run_id) -> bool` — conditional liveness refresh for the
    stale-run sweeper (`openspec/changes/agent-stale-run-sweeper`); returns
    False once the row is terminal.
  - `sweep_stale_runs(timeout_seconds) -> [uuid]` — reaps zombie `running`
    rows to `failed`/`stale_run`.
  - `retention_delete_terminal(days) -> [uuid]` — deletes expired terminal
    rows and returns their ids so the retention cron can GC their langGraph
    checkpoint threads.
  - `delete_all_for_user(user_id) -> [uuid]` — owner-scoped erasure (GDPR
    right-to-erasure, openspec/changes/agent-gdpr-compliance D3): deletes
    every `agent.runs` row for one user and returns the run ids so the
    endpoint can drive `adelete_thread` for each checkpoint thread.
  - `list_all_runs_for_user(user_id)` — unpaginated export read for the
    same change, oldest-first.

State persistence itself is handled by langGraph's `AsyncPostgresSaver` (in
`langgraph.*`). This module is the *metadata* mirror, not the state store.

Defence in depth: every read includes `user_id` in the SQL `WHERE` clause.
The upstream `X-Nyx-User-Id` header is the primary authentication; this
filter is the contract.

Retry policy: transient `asyncpg.PostgresConnectionError` triggers 3-retry
exponential backoff (100ms, 200ms, 400ms). Sustained failures raise
`RunMetadataUnavailable`; the SSE consumer translates to
`error{code:"run_metadata_unavailable"}` and continues the run.
"""
from __future__ import annotations

import asyncio
import logging
import uuid as uuid_lib
from dataclasses import dataclass
from datetime import datetime
from typing import Any

import asyncpg

log = logging.getLogger("nyx.agent.run_metadata")

# SQL column list shared between methods — keeps the projection explicit and
# stable; schema migrations only need to update this string when adding columns.
_RUN_COLUMNS = "id, user_id, status, total_tokens, error_code, started_at, ended_at"


class RunMetadataUnavailable(RuntimeError):
    """Raised when metadata operations fail after the retry budget is exhausted."""


@dataclass(slots=True)
class RunSummary:
    id: str
    user_id: int
    status: str
    total_tokens: int
    error_code: str | None
    started_at: datetime
    ended_at: datetime | None


class RunMetadata:
    """Thin async wrapper over `agent.runs`. Connection pool owned by the caller."""

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

        Pool-acquire timeout (asyncio.TimeoutError from
        `pool.acquire(timeout=...)`) is a separate failure mode — the
        pool is exhausted and retrying within the same request won't
        help — so it raises `RunMetadataUnavailable` immediately per
        the spec ("Pool acquire timeout SHALL be 5s; exceeding it
        SHALL raise `RunMetadataUnavailable`").
        """
        attempt = 0
        backoff_ms = 100
        while True:
            try:
                async with self._pool.acquire(timeout=self._acquire_timeout) as conn:
                    return await fn(conn)
            except asyncio.TimeoutError as exc:
                log.warning(
                    "run_metadata %s: pool acquire timeout after %.1fs (%r)",
                    op_name,
                    self._acquire_timeout,
                    exc,
                )
                raise RunMetadataUnavailable(
                    f"{op_name} failed: pool acquire timeout "
                    f"({self._acquire_timeout}s)"
                ) from exc
            except asyncpg.PostgresConnectionError as exc:
                attempt += 1
                if attempt > self._max_retries:
                    log.warning(
                        "run_metadata %s: retries exhausted (%r)", op_name, exc
                    )
                    raise RunMetadataUnavailable(
                        f"{op_name} failed after {self._max_retries} retries"
                    ) from exc
                log.info(
                    "run_metadata %s: retry %d/%d after %dms (%r)",
                    op_name,
                    attempt,
                    self._max_retries,
                    backoff_ms,
                    exc,
                )
                await asyncio.sleep(backoff_ms / 1000)
                backoff_ms *= 2

    async def create_run(self, user_id: int) -> str:
        """Mint a uuid4 server-side and INSERT a fresh `agent.runs` row.

        Returns the new `run_id` as a canonical uuid4 string.
        """

        async def _insert(conn: asyncpg.Connection) -> str:
            run_id = str(uuid_lib.uuid4())
            await conn.execute(
                "INSERT INTO runs (id, user_id, status, last_heartbeat_at) "
                "VALUES ($1, $2, 'running', now())",
                run_id,
                user_id,
            )
            return run_id

        return await self._with_retry("create_run", _insert)

    async def mark_completed(self, run_id: str, total_tokens: int) -> None:
        # Terminal writes are unconditional and authoritative (the live
        # process knows the run's true outcome) — and `error_code=NULL`
        # clears a `stale_run` written by the sweeper against a stalled
        # heartbeat, so a premature reap cannot outlive a real completion
        # (openspec/changes/agent-stale-run-sweeper, D3).
        async def _update(conn: asyncpg.Connection) -> None:
            await conn.execute(
                "UPDATE runs SET status='completed', ended_at=now(), "
                "total_tokens=$2, error_code=NULL "
                "WHERE id=$1",
                run_id,
                total_tokens,
            )

        await self._with_retry("mark_completed", _update)

    async def mark_failed(self, run_id: str, error_code: str) -> None:
        async def _update(conn: asyncpg.Connection) -> None:
            await conn.execute(
                "UPDATE runs SET status='failed', ended_at=now(), error_code=$2 "
                "WHERE id=$1",
                run_id,
                error_code,
            )

        await self._with_retry("mark_failed", _update)

    async def touch_heartbeat(self, run_id: str) -> bool:
        """Refresh `last_heartbeat_at` for a live run (stale-run sweeper).

        Conditional on the row still being `running`, so a heartbeat can
        never resurrect a terminal row (see the sweeper's first-writer-wins
        semantics in `openspec/changes/agent-stale-run-sweeper`). Returns
        True when the row was touched; False means the row is already
        terminal or gone — a non-exceptional outcome the caller treats as
        "stop heartbeating", not as an error.
        """

        async def _update(conn: asyncpg.Connection) -> bool:
            tag = await conn.execute(
                "UPDATE runs SET last_heartbeat_at=now() "
                "WHERE id=$1 AND status='running'",
                run_id,
            )
            return _parse_command_tag(tag) == 1

        return await self._with_retry("touch_heartbeat", _update)

    async def get_run(self, run_id: str, user_id: int) -> RunSummary | None:
        """Defensive per-user read. Returns None for not-found / wrong-user / wrong-schema."""

        async def _select(conn: asyncpg.Connection) -> RunSummary | None:
            row = await conn.fetchrow(
                f"SELECT {_RUN_COLUMNS} FROM runs WHERE id=$1 AND user_id=$2",
                run_id,
                user_id,
            )
            return _row_to_summary(row) if row else None

        return await self._with_retry("get_run", _select)

    async def list_runs_for_user(
        self, user_id: int, *, limit: int, offset: int
    ) -> list[RunSummary]:
        """Paginated user-scoped history. Backed by `runs_user_started_idx`."""

        async def _select(conn: asyncpg.Connection) -> list[RunSummary]:
            rows = await conn.fetch(
                f"SELECT {_RUN_COLUMNS} FROM runs WHERE user_id=$1 "
                "ORDER BY started_at DESC LIMIT $2 OFFSET $3",
                user_id,
                limit,
                offset,
            )
            return [_row_to_summary(r) for r in rows]

        return await self._with_retry("list_runs_for_user", _select)

    async def list_all_runs_for_user(self, user_id: int) -> list[RunSummary]:
        """Export read: every run row for the user, unpaginated, oldest-first.

        Sibling of `list_runs_for_user` for the GDPR export
        (openspec/changes/agent-gdpr-erasure): no LIMIT/OFFSET (export
        completeness) and `(started_at ASC, id ASC)` ordering for a
        deterministic dump — the `id` tiebreaker keeps same-microsecond
        rows in insertion order.
        """

        async def _select(conn: asyncpg.Connection) -> list[RunSummary]:
            rows = await conn.fetch(
                f"SELECT {_RUN_COLUMNS} FROM runs WHERE user_id=$1 "
                "ORDER BY started_at ASC, id ASC",
                user_id,
            )
            return [_row_to_summary(r) for r in rows]

        return await self._with_retry("list_all_runs_for_user", _select)

    async def retention_delete_terminal(self, retention_days: int) -> list[str]:
        """Delete completed/failed runs older than `retention_days`. Returns the deleted run ids.

        Hard delete only — `archived_at` column reserved for a future soft-delete
        + GDPR-erase split. The returned ids drive the langGraph
        checkpoint-thread GC in the retention cron (`adelete_thread`); this
        method itself touches only `agent.runs`. langGraph ships no retention
        of its own, so that GC call is the only path that prunes
        `langgraph.checkpoint_*` (the sweeper deliberately preserves
        checkpoint data — see openspec/changes/agent-stale-run-sweeper).
        """

        async def _delete(conn: asyncpg.Connection) -> list[str]:
            # `now() - ($1 * interval '1 day')` is the canonical "N days
            # ago" form in Postgres; works for any integer parameter
            # without string-concatenation (which would force asyncpg to
            # try to determine the literal type of `'$2 || days'` and
            # fail with IndeterminateDatatypeError).
            rows = await conn.fetch(
                "DELETE FROM runs "
                "WHERE ended_at < now() - ($1 * interval '1 day') "
                "AND status IN ('completed', 'failed') "
                "RETURNING id",
                retention_days,
            )
            return [str(r["id"]) for r in rows]

        return await self._with_retry("retention_delete_terminal", _delete)

    async def delete_all_for_user(self, user_id: int) -> list[str]:
        """Owner-scoped erasure: delete EVERY `agent.runs` row for the user.

        The GDPR right-to-erasure path (openspec/changes/agent-gdpr-compliance
        D3/D7) — unlike `retention_delete_terminal` this ignores `status`:
        an erasure request deletes running, completed, and failed rows
        alike. Returns the deleted run ids (canonical uuid strings) so the
        endpoint can drive `adelete_thread(run_id)` for each langGraph
        checkpoint thread — the thread id IS the run id, so this list is
        the complete GC work-list. Touches only `agent.runs`; the
        checkpoint delete is the caller's step (per-thread failures are
        contained there). A second call returns `[]` (idempotent).
        """

        async def _delete(conn: asyncpg.Connection) -> list[str]:
            rows = await conn.fetch(
                "DELETE FROM runs WHERE user_id=$1 RETURNING id",
                user_id,
            )
            return [str(r["id"]) for r in rows]

        return await self._with_retry("delete_all_for_user", _delete)

    async def sweep_stale_runs(self, timeout_seconds: int) -> list[str]:
        """Reap zombie `running` rows (stale-run sweeper). Returns the reaped run ids.

        Transitions every row matching `status='running' AND
        last_heartbeat_at < now() - timeout` to `status='failed'`,
        `error_code='stale_run'`, `ended_at=now()`. The `status='running'`
        predicate makes the update first-writer-wins: a row driven terminal
        between the sweeper's scan and its update is never modified, and a
        live process's `mark_completed`/`mark_failed` stays authoritative.
        """

        async def _update(conn: asyncpg.Connection) -> list[str]:
            rows = await conn.fetch(
                "UPDATE runs SET status='failed', error_code='stale_run', "
                "ended_at=now() "
                "WHERE status='running' "
                "AND last_heartbeat_at < now() - ($1 * interval '1 second') "
                "RETURNING id",
                timeout_seconds,
            )
            return [str(r["id"]) for r in rows]

        return await self._with_retry("sweep_stale_runs", _update)


def _row_to_summary(row: asyncpg.Record | Any) -> RunSummary:
    return RunSummary(
        id=row["id"],
        user_id=row["user_id"],
        status=row["status"],
        total_tokens=row["total_tokens"],
        error_code=row["error_code"],
        started_at=row["started_at"],
        ended_at=row["ended_at"],
    )


def _parse_command_tag(tag: str) -> int:
    """asyncpg.execute returns the Postgres command tag, e.g. 'DELETE 3'."""
    parts = tag.rsplit(" ", 1)
    if len(parts) != 2:
        return 0
    try:
        return int(parts[1])
    except ValueError:
        return 0


async def build_pool(database_url: str, *, max_size: int = 10) -> asyncpg.Pool:
    """Build an *initialized* asyncpg pool from the configured DSN.

    asyncpg 0.32+ changed the API: `asyncpg.create_pool()` returns a Pool
    whose `_initialized` flag is False; the pool only completes its init
    when you `await` it (calls `Pool.__await__` → `_async__init__`) or
    `async with pool:`. Without that, the first `acquire()` raises
    `InterfaceError("pool is not initialized")`.

    Caller is responsible for `await pool.close()` on shutdown.
    """
    pool = asyncpg.create_pool(
        dsn=database_url,
        min_size=1,
        max_size=max_size,
        command_timeout=30.0,
    )
    # `await pool` invokes Pool.__await__ → _async__init__ → sets _initialized.
    return await pool
