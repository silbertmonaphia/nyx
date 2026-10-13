"""Thin metadata layer for agent runs.

Owns `agent.runs` (a Postgres table in the `agent.*` schema) for user-facing
queries that langGraph's opaque `checkpoint_*` blob doesn't expose:

  - `list_runs_for_user(user_id, limit, offset)` — paginated history for the
    "show me my runs" UX.
  - `mark_completed` / `mark_failed` — terminal state transitions.
  - `create_run(user_id) -> uuid` — mints a server-side uuid4 for fresh runs.
  - `get_run(run_id, user_id)` — defensive per-user read used by the resume
    decision in `main.py`.

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
                "INSERT INTO runs (id, user_id, status) VALUES ($1, $2, 'running')",
                run_id,
                user_id,
            )
            return run_id

        return await self._with_retry("create_run", _insert)

    async def mark_completed(self, run_id: str, total_tokens: int) -> None:
        async def _update(conn: asyncpg.Connection) -> None:
            await conn.execute(
                "UPDATE runs SET status='completed', ended_at=now(), total_tokens=$2 "
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

    async def retention_delete_terminal(self, retention_days: int) -> int:
        """Delete completed/failed runs older than `retention_days`. Returns row count.

        Hard delete only — `archived_at` column reserved for a future soft-delete
        + GDPR-erase split. langGraph checkpoint tables are pruned via langGraph's
        own retention story; this method touches only `agent.runs`.
        """

        async def _delete(conn: asyncpg.Connection) -> int:
            # `now() - ($1 * interval '1 day')` is the canonical "N days
            # ago" form in Postgres; works for any integer parameter
            # without string-concatenation (which would force asyncpg to
            # try to determine the literal type of `'$2 || days'` and
            # fail with IndeterminateDatatypeError).
            result = await conn.execute(
                "DELETE FROM runs "
                "WHERE ended_at < now() - ($1 * interval '1 day') "
                "AND status IN ('completed', 'failed')",
                retention_days,
            )
            # asyncpg returns "DELETE <n>".
            return _parse_command_tag(result)

        return await self._with_retry("retention_delete_terminal", _delete)


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
