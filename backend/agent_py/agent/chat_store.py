"""Thin metadata layer for chat sessions and messages.

Owns `agent.chat_sessions` and `agent.chat_messages` (Postgres tables in
the `agent.*` schema) for user-facing chat history:

  - `create_session(user_id, run_id)` — INSERT a session row at run start
  - `get_session(session_id, user_id)` — defensive per-user read used by
    the resume decision (Slice 2 will reuse this)
  - `list_sessions(user_id, limit, before)` — one sidebar page: newest-
    first `SessionSummary` rows with their `message_count` (single
    `LEFT JOIN ... GROUP BY` — no N+1) plus the next-page cursor
    (`openspec/changes/agent-chat-history-ui`)
  - `list_messages(session_id, user_id)` — chronological replay of one
    session's messages; `None` when the session is unknown / foreign /
    soft-deleted (the API maps that to 404)
  - `record_message(session_id, user_id, role, content, tool_calls, tokens)`
    — append one row to `chat_messages` and bump `last_active_at` on the
    parent session (D8 in `design.md`)
  - `touch_session(session_id, user_id)` — refresh `last_active_at`
    without inserting a message row
  - `soft_delete_session(session_id, user_id)` — user-initiated delete:
    `deleted_at = now()`, messages stay until the retention cron
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
import base64
import json as _json
import logging
import uuid as uuid_lib
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
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


@dataclass(slots=True)
class SessionSummary:
    """One sidebar row: session metadata plus its message count.

    `message_count` is computed by the list query itself
    (`LEFT JOIN ... count(m.id)`, one round-trip — no N+1); an empty
    session reads 0.
    """

    id: str | uuid_lib.UUID
    user_id: int
    title: str
    started_at: datetime
    last_active_at: datetime
    ended_at: datetime | None
    message_count: int


@dataclass(slots=True)
class MessageRow:
    """One replayed message. `tool_calls` is the decoded JSON array (or None)."""

    id: int
    role: str
    content: str
    tool_calls: list[dict[str, Any]] | None
    tokens: int
    created_at: datetime


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

    async def list_sessions(
        self,
        user_id: int,
        *,
        limit: int,
        before: tuple[datetime, str] | None = None,
    ) -> tuple[list[SessionSummary], str | None]:
        """One sidebar page: newest-first summaries + the next-page cursor.

        Single statement per page — the `LEFT JOIN chat_messages ...
        GROUP BY s.id` computes every row's `message_count` in one
        round-trip (design.md risk "N+1 on `message_count`"). The count
        is `count(m.id)`, never `count(*)`: the LEFT JOIN emits one
        all-NULL message row for a session with no messages, which
        `count(*)` would count as 1.

        `before` is the decoded `(last_active_at, id)` tuple of the last
        row of the previous page (the API layer decodes the cursor); the
        tuple comparison is strict (`<`) so the boundary row is never
        repeated. Order is `(last_active_at DESC, id DESC)` — same
        microsecond, higher id first.

        Fetches `limit + 1` rows: a full extra row proves there is
        another page, so it is dropped and `next_cursor` encodes the
        LAST INCLUDED row's tuple (design.md D1 — base64url of
        `<unix-microseconds>_<uuid>`). `next_cursor=None` means this was
        the last page; callers must not re-issue the request.
        """
        fetch_limit = limit + 1
        select_from = (
            "SELECT s.id, s.user_id, s.title, s.started_at, s.last_active_at, "
            "s.ended_at, count(m.id) AS message_count "
            "FROM chat_sessions s "
            "LEFT JOIN chat_messages m ON m.session_id = s.id "
            "WHERE s.user_id=$1 AND s.deleted_at IS NULL "
        )
        group_order = (
            "GROUP BY s.id "
            "ORDER BY s.last_active_at DESC, s.id DESC "
        )
        if before is None:
            sql = select_from + group_order + "LIMIT $2"
            args: tuple[Any, ...] = (user_id, fetch_limit)
        else:
            # Explicit casts: a row-constructor comparison gives asyncpg
            # no column context to infer the parameter types from.
            sql = (
                select_from
                + "AND (s.last_active_at, s.id) < ($2::timestamptz, $3::uuid) "
                + group_order
                + "LIMIT $4"
            )
            args = (user_id, before[0], before[1], fetch_limit)

        async def _select(
            conn: asyncpg.Connection,
        ) -> tuple[list[SessionSummary], str | None]:
            rows = await conn.fetch(sql, *args)
            page = rows[:limit]
            summaries = [_row_to_summary(r) for r in page]
            next_cursor = None
            if len(rows) > limit and page:
                last = page[-1]
                next_cursor = _encode_cursor(last["last_active_at"], last["id"])
            return summaries, next_cursor

        return await self._with_retry("list_sessions", _select)

    async def list_messages(
        self, session_id: str | uuid_lib.UUID, user_id: int
    ) -> list[MessageRow] | None:
        """Chronological replay of one session's messages, or None.

        `None` means "no such session for this user" — unknown id, wrong
        user, or soft-deleted — which the API maps to 404 (design.md
        risk "Cross-user leak via 404 vs 403": the response must not
        distinguish those cases, so the API resolves them all to the
        same 404). An existing session with no messages returns `[]`.

        Two statements in ONE transaction (review M3, 2026-10-14): the
        user-scoped ownership probe (cheap, hits the pk index), then the
        message read ordered `(created_at ASC, id ASC)` — the `id`
        tiebreaker keeps same-microsecond messages in insertion order.
        The read is keyed on the probe-validated id; the transaction
        keeps a concurrent retention purge from slipping between the
        probe and the read (which would turn a would-be 404 into `[]`).

        `tool_calls::text` + `json.loads`: asyncpg hands jsonb back as a
        raw string by default, and the wire contract wants a JSON array
        (or null), not a quoted string.
        """

        async def _select(conn: asyncpg.Connection) -> list[MessageRow] | None:
            async with conn.transaction():
                exists = await conn.fetchval(
                    "SELECT 1 FROM chat_sessions "
                    "WHERE id=$1 AND user_id=$2 AND deleted_at IS NULL",
                    str(session_id),
                    user_id,
                )
                if exists is None:
                    return None
                rows = await conn.fetch(
                    "SELECT id, role, content, tool_calls::text AS tool_calls, "
                    "tokens, created_at FROM chat_messages "
                    "WHERE session_id=$1 "
                    "ORDER BY created_at ASC, id ASC",
                    str(session_id),
                )
                return [_row_to_message(r) for r in rows]

        return await self._with_retry("list_messages", _select)

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

    async def soft_delete_session(
        self, session_id: str | uuid_lib.UUID, user_id: int
    ) -> bool:
        """User-initiated soft-delete: `deleted_at = now()`.

        The retention cron owns hard-delete (`hard_delete_soft_deleted`);
        until it reaps the row, both the session and its messages stay in
        the table (design.md D3) but every read path filters them out
        (`deleted_at IS NULL`). Returns True on the first delete, False
        when the row is unknown / foreign / already soft-deleted — the
        caller maps False to 404, which also makes a second DELETE on the
        same id a 404 (the row is invisible from then on).
        """

        async def _update(conn: asyncpg.Connection) -> bool:
            tag = await conn.execute(
                "UPDATE chat_sessions SET deleted_at=now() "
                "WHERE id=$1 AND user_id=$2 AND deleted_at IS NULL",
                str(session_id),
                user_id,
            )
            return _parse_command_tag(tag) == 1

        return await self._with_retry("soft_delete_session", _update)

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


def _row_to_summary(row: asyncpg.Record | Any) -> SessionSummary:
    return SessionSummary(
        id=row["id"],
        user_id=row["user_id"],
        title=row["title"],
        started_at=row["started_at"],
        last_active_at=row["last_active_at"],
        ended_at=row["ended_at"],
        message_count=row["message_count"],
    )


def _row_to_message(row: asyncpg.Record | Any) -> MessageRow:
    raw_tool_calls = row["tool_calls"]
    return MessageRow(
        id=row["id"],
        role=row["role"],
        content=row["content"],
        tool_calls=(
            _json.loads(raw_tool_calls) if raw_tool_calls is not None else None
        ),
        tokens=row["tokens"],
        created_at=row["created_at"],
    )


_EPOCH = datetime(1970, 1, 1, tzinfo=timezone.utc)


def _encode_cursor(last_active_at: datetime, session_id: Any) -> str:
    """Encode a row's `(last_active_at, id)` as base64url `<unix-microseconds>_<uuid>`.

    Design.md D1. Padding is stripped (RFC 4648 §5) so the token rides in
    a query string as-is; the decoder re-pads. Microseconds are computed
    with integer timedelta arithmetic — `timestamp() * 1_000_000`
    round-trips through a float whose resolution at 2026-epoch magnitudes
    is coarser than 1µs, and a cursor even 1µs below the row it
    describes would make the next page's strict `<` comparison skip a row
    that shares that microsecond.
    """
    micros = (last_active_at - _EPOCH) // timedelta(microseconds=1)
    raw = f"{micros}_{session_id}"
    return base64.urlsafe_b64encode(raw.encode("ascii")).decode("ascii").rstrip("=")


def _parse_command_tag(tag: str) -> int:
    """asyncpg.execute returns the Postgres command tag, e.g. 'UPDATE 1' or 'DELETE 3'."""
    parts = tag.rsplit(" ", 1)
    if len(parts) != 2:
        return 0
    try:
        return int(parts[1])
    except ValueError:
        return 0
