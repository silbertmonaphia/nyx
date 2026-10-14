"""Thin metadata layer for chat sessions and messages.

Owns `agent.chat_sessions` and `agent.chat_messages` (Postgres tables in
the `agent.*` schema) for user-facing chat history:

  - `create_session(user_id, run_id)` — INSERT a session row at run start
  - `get_session(session_id, user_id)` — defensive per-user read used by
    the resume decision (Slice 2 will reuse this)
  - `record_message(session_id, user_id, role, content, tool_calls, tokens)`
    — append one row to `chat_messages` and bump `last_active_at` on the
    parent session (D8 in `design.md`)
  - `touch_session(session_id, user_id)` — refresh `last_active_at`
    without inserting a message row
  - `end_session(session_id, user_id)` — set `ended_at = now()` on
    termination
  - `soft_delete_expired(retention_days) -> list[str]` — first phase of
    the two-phase retention: set `deleted_at = now()` on sessions past
    the retention window; return the soft-deleted ids so the caller can
    audit
  - `hard_delete_soft_deleted(grace_days) -> int` — second phase: delete
    rows whose `deleted_at` is past the grace window; returns the count
    so the caller can increment the metric. `chat_messages` cascades via
    FK ON DELETE CASCADE — a single DELETE on `chat_sessions` cleans
    the messages too.

Same retry posture as `RunMetadata`: 100ms→200ms→400ms exponential
backoff for transient `asyncpg.PostgresConnectionError`; pool acquire
timeout raises `ChatStoreUnavailable` immediately (5s default).

Defence in depth: every read/write that takes a session_id MUST also
take `user_id` and include it in the SQL `WHERE` clause. The upstream
`X-Nyx-User-Id` header is the primary authentication; this filter is
the contract (matches `RunMetadata`'s posture).
"""
from __future__ import annotations

import asyncio
import logging
import uuid as uuid_lib
from dataclasses import dataclass
from datetime import datetime
from typing import Any

import asyncpg

log = logging.getLogger("nyx.agent.chat_store")


class ChatStoreUnavailable(RuntimeError):
    """Raised when chat-store operations fail after the retry budget is exhausted."""


@dataclass(slots=True)
class ChatSession:
    id: str
    user_id: int
    title: str
    started_at: datetime
    last_active_at: datetime
    ended_at: datetime | None
    deleted_at: datetime | None


class ChatStore:
    """Thin async wrapper over `agent.chat_sessions` and `agent.chat_messages`.

    Connection pool owned by the caller (reused from `RunMetadata`'s
    pool — see design.md D9; no second pool).
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

        Pool-acquire timeout (asyncio.TimeoutError from
        `pool.acquire(timeout=...)`) is a separate failure mode — the
        pool is exhausted and retrying within the same request won't
        help — so it raises `ChatStoreUnavailable` immediately (matches
        `RunMetadata._with_retry`).
        """
        attempt = 0
        backoff_ms = 100
        while True:
            try:
                async with self._pool.acquire(timeout=self._acquire_timeout) as conn:
                    return await fn(conn)
            except asyncio.TimeoutError as exc:
                log.warning(
                    "chat_store %s: pool acquire timeout after %.1fs (%r)",
                    op_name,
                    self._acquire_timeout,
                    exc,
                )
                raise ChatStoreUnavailable(
                    f"{op_name} failed: pool acquire timeout "
                    f"({self._acquire_timeout}s)"
                ) from exc
            except asyncpg.PostgresConnectionError as exc:
                attempt += 1
                if attempt > self._max_retries:
                    log.warning(
                        "chat_store %s: retries exhausted (%r)", op_name, exc
                    )
                    raise ChatStoreUnavailable(
                        f"{op_name} failed after {self._max_retries} retries"
                    ) from exc
                log.info(
                    "chat_store %s: retry %d/%d after %dms (%r)",
                    op_name,
                    attempt,
                    self._max_retries,
                    backoff_ms,
                    exc,
                )
                await asyncio.sleep(backoff_ms / 1000)
                backoff_ms *= 2

    async def create_session(
        self,
        user_id: int,
        run_id: str | uuid_lib.UUID,
        *,
        title: str = "",
    ) -> None:
        """INSERT a fresh `agent.chat_sessions` row.

        `ON CONFLICT (id) DO NOTHING` makes the call idempotent for
        resumed runs: if the session row already exists (the run was
        created in a prior session/process and the metadata says it's
        resumable but the chat_sessions row was, e.g., soft-deleted
        between processes), this still inserts a fresh row when the id
        is free and is a no-op when it's not. The downstream `touch_session`
        then refreshes `last_active_at` regardless.

        The original "call only on fresh runs" contract held for
        single-process runs; this `ON CONFLICT` clause is the
        defence-in-depth for cross-process resumes where the metadata
        row survives but the chat_sessions row may not.
        """
        run_id_str = str(run_id)

        async def _insert(conn: asyncpg.Connection) -> None:
            await conn.execute(
                "INSERT INTO chat_sessions (id, user_id, title) "
                "VALUES ($1, $2, $3) "
                "ON CONFLICT (id) DO NOTHING",
                run_id_str,
                user_id,
                title,
            )

        await self._with_retry("create_session", _insert)

    async def get_session(
        self, session_id: str | uuid_lib.UUID, user_id: int
    ) -> ChatSession | None:
        """Defensive per-user read. Returns None for not-found / wrong-user."""

        async def _select(conn: asyncpg.Connection) -> ChatSession | None:
            row = await conn.fetchrow(
                "SELECT id, user_id, title, started_at, last_active_at, "
                "ended_at, deleted_at FROM chat_sessions "
                "WHERE id=$1 AND user_id=$2 AND deleted_at IS NULL",
                str(session_id),
                user_id,
            )
            return _row_to_session(row) if row else None

        return await self._with_retry("get_session", _select)

    async def record_message(
        self,
        session_id: str | uuid_lib.UUID,
        user_id: int,
        role: str,
        content: str,
        *,
        tool_calls: list[dict[str, Any]] | None = None,
        tokens: int = 0,
    ) -> int:
        """Append one row to `chat_messages` and bump `last_active_at`.

        Returns the new message id (bigserial). Touches the parent
        session's `last_active_at` as a single statement to keep the
        sidebar index current; a separate UPDATE would double the
        round-trip cost on the chat hot path (D6 in `design.md`).

        Cross-user isolation: the WHERE clause on the touch enforces
        `user_id` even though the FK already guarantees the session
        exists — defence in depth, matches `RunMetadata`'s posture.
        """
        session_id_str = str(session_id)
        import json as _json

        # jsonb: serialise via asyncpg's set_encoder; raw JSON string is
        # accepted by the JSONB column cast.
        tool_calls_json = (
            _json.dumps(tool_calls) if tool_calls is not None else None
        )

        async def _tx(conn: asyncpg.Connection) -> int:
            async with conn.transaction():
                message_id: int = await conn.fetchval(
                    "INSERT INTO chat_messages "
                    "(session_id, role, content, tool_calls, tokens) "
                    "VALUES ($1, $2, $3, $4::jsonb, $5) "
                    "RETURNING id",
                    session_id_str,
                    role,
                    content,
                    tool_calls_json,
                    tokens,
                )
                await conn.execute(
                    "UPDATE chat_sessions SET last_active_at=now() "
                    "WHERE id=$1 AND user_id=$2 AND deleted_at IS NULL",
                    session_id_str,
                    user_id,
                )
            return message_id

        return await self._with_retry("record_message", _tx)

    async def touch_session(
        self, session_id: str | uuid_lib.UUID, user_id: int
    ) -> bool:
        """Refresh `last_active_at` without inserting a message row.

        Used on resumed runs (echoed `run_id` already in `chat_sessions`)
        to mark the session active again. Returns True on touch, False
        when the row doesn't exist for this user (or is soft-deleted) —
        a non-exceptional outcome the caller treats as "stop tracking",
        not as an error.
        """

        async def _update(conn: asyncpg.Connection) -> bool:
            tag = await conn.execute(
                "UPDATE chat_sessions SET last_active_at=now() "
                "WHERE id=$1 AND user_id=$2 AND deleted_at IS NULL",
                str(session_id),
                user_id,
            )
            return _parse_command_tag(tag) == 1

        return await self._with_retry("touch_session", _update)

    async def end_session(
        self,
        session_id: str | uuid_lib.UUID,
        user_id: int,
        *,
        ended_at: datetime | None = None,
    ) -> bool:
        """Set `ended_at` on the session. Idempotent.

        The retention cron uses `last_active_at` as its predicate (D8),
        so `end_session` is purely informational for now — Slice 2's
        sidebar can use it to distinguish "live" from "ended" sessions.
        Returns True on update, False when the row doesn't exist for
        this user.
        """

        async def _update(conn: asyncpg.Connection) -> bool:
            # `COALESCE(ended_at, COALESCE($3, now()))` preserves an
            # already-set ended_at on a repeat call (idempotent), and
            # picks the caller-supplied value or now() on the first
            # call. Plain `COALESCE($3, now())` would always overwrite
            # the column with now(), breaking idempotency.
            tag = await conn.execute(
                "UPDATE chat_sessions "
                "SET ended_at=COALESCE(ended_at, COALESCE($3, now())) "
                "WHERE id=$1 AND user_id=$2 AND deleted_at IS NULL",
                str(session_id),
                user_id,
                ended_at,
            )
            return _parse_command_tag(tag) == 1

        return await self._with_retry("end_session", _update)

    async def soft_delete_expired(self, retention_days: int) -> list[str]:
        """Mark sessions past `retention_days` as soft-deleted.

        First phase of the two-phase retention (D4): set
        `deleted_at = now()` on rows whose `last_active_at` is older
        than the retention window. Returns the soft-deleted session ids
        (caller may use for audit logs; the metric counts hard-deletes
        only, not these).

        `now() - ($1 * interval '1 day')` is the canonical "N days ago"
        form in Postgres; works for any integer parameter without
        string-concatenation (which would force asyncpg to try to
        determine the literal type of `'$2 || days'` and fail with
        IndeterminateDatatypeError).
        """

        async def _update(conn: asyncpg.Connection) -> list[str]:
            rows = await conn.fetch(
                "UPDATE chat_sessions SET deleted_at=now() "
                "WHERE last_active_at < now() - ($1 * interval '1 day') "
                "AND deleted_at IS NULL "
                "RETURNING id",
                retention_days,
            )
            return [str(r["id"]) for r in rows]

        return await self._with_retry("soft_delete_expired", _update)

    async def hard_delete_soft_deleted(self, grace_days: int) -> int:
        """Delete sessions whose `deleted_at` is past the grace window.

        Second phase of the two-phase retention. Returns the count of
        sessions hard-deleted (caller increments
        `agent_chat_retention_deleted_total` by this value).

        `chat_messages` cascades via `ON DELETE CASCADE` on the FK, so
        one statement cleans both tables.
        """

        async def _delete(conn: asyncpg.Connection) -> int:
            rows = await conn.fetch(
                "DELETE FROM chat_sessions "
                "WHERE deleted_at IS NOT NULL "
                "AND deleted_at < now() - ($1 * interval '1 day') "
                "RETURNING id",
                grace_days,
            )
            return len(rows)

        return await self._with_retry("hard_delete_soft_deleted", _delete)


def _row_to_session(row: asyncpg.Record | Any) -> ChatSession:
    return ChatSession(
        id=row["id"],
        user_id=row["user_id"],
        title=row["title"],
        started_at=row["started_at"],
        last_active_at=row["last_active_at"],
        ended_at=row["ended_at"],
        deleted_at=row["deleted_at"],
    )


def _parse_command_tag(tag: str) -> int:
    """asyncpg.execute returns the Postgres command tag, e.g. 'UPDATE 1' or 'DELETE 3'."""
    parts = tag.rsplit(" ", 1)
    if len(parts) != 2:
        return 0
    try:
        return int(parts[1])
    except ValueError:
        return 0
