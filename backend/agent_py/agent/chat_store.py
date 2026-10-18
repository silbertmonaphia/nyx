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
  - `record_message(session_id, user_id, role, content, tool_calls, steps,
    tokens, prompt_tokens, completion_tokens)` — append one row to
    `chat_messages` and bump `last_active_at` on the parent session
    (D8 in `design.md`)
  - `touch_session(session_id, user_id)` — refresh `last_active_at`
    without inserting a message row
  - `set_title_if_empty(session_id, user_id, title)` — first-exchange
    deterministic title, never clobbering an existing one
    (`openspec/changes/agent-chat-fixes` D2)
  - `set_title(session_id, user_id, title)` — overwrite for the async
    refinement of that title
  - `soft_delete_session(session_id, user_id)` — user-initiated delete:
    `deleted_at = now()`, messages stay until the retention cron
  - `end_session(session_id, user_id)` — set `ended_at = now()` on
    termination
  - `soft_delete_expired(retention_days) -> list[str]` — first phase of
    the two-phase retention: set `deleted_at = now()` on sessions past
    the retention window; return the soft-deleted ids so the caller can
    audit
  - `hard_delete_soft_deleted(grace_days) -> list[str]` — second phase:
    delete rows whose `deleted_at` is past the grace window; returns the
    deleted session ids so the caller can increment the metric by
    `len(ids)` and clean up derived rows (prefs). `chat_messages`
    cascades via FK ON DELETE CASCADE — a single DELETE on
    `chat_sessions` cleans the messages too.
  - `delete_all_for_user(user_id) -> dict[str, Any]` — owner-scoped
    erasure (GDPR right-to-erasure, openspec/changes/agent-gdpr-compliance
    D3/D7): one transaction deleting messages → sessions → prefs for one
    user, soft-deleted rows INCLUDED (retention is not a precondition for
    erasure). Returns `{messages, sessions, prefs, session_ids}` — the ids
    double as langGraph thread ids so the erase endpoint can GC
    session-keyed threads.
  - `list_all_sessions_for_user(user_id)` / `list_all_messages_for_user(user_id)`
    — unpaginated export reads for the same change, soft-deleted rows
    INCLUDED (export completeness), oldest-first.

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
from datetime import UTC, datetime, timedelta
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
    """One replayed message.

    `tool_calls` and `steps` are the decoded JSON arrays (or None).
    `steps` carries the assistant row's step trace; it stays None for
    rows persisted before the column existed, non-assistant rows, and
    runs that planned no steps.

    `session_id` (string form) links the message to its conversation —
    needed by the GDPR export dump so message rows are interpretable
    without cross-referencing (review 2026-10-14: the export shipped
    session rows and message rows side by side with no join key). It is
    additive on the dataclass only; the `/agent/sessions/{id}/messages`
    replay wire still emits its documented field set (review L4).
    """

    id: int
    role: str
    content: str
    tool_calls: list[dict[str, Any]] | None
    tokens: int
    prompt_tokens: int | None
    completion_tokens: int | None
    created_at: datetime
    session_id: str
    steps: list[dict[str, Any]] | None = None


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
            except TimeoutError as exc:
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

        `tool_calls::text` / `steps::text` + `json.loads`: asyncpg hands
        jsonb back as a raw string by default, and the wire contract
        wants a JSON array (or null), not a quoted string.
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
                    "SELECT id, session_id, role, content, "
                    "tool_calls::text AS tool_calls, "
                    "steps::text AS steps, "
                    "tokens, prompt_tokens, completion_tokens, "
                    "created_at FROM chat_messages "
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
        steps: list[dict[str, Any]] | None = None,
        tokens: int = 0,
        prompt_tokens: int | None = None,
        completion_tokens: int | None = None,
    ) -> int:
        """Append one row to `chat_messages` and bump `last_active_at`.

        Returns the new message id (bigserial). Touches the parent
        session's `last_active_at` as a single statement to keep the
        sidebar index current; a separate UPDATE would double the
        round-trip cost on the chat hot path (D6 in `design.md`).

        Cross-user isolation: the WHERE clause on the touch enforces
        `user_id` even though the FK already guarantees the session
        exists — defence in depth, matches `RunMetadata`'s posture.

        Disappearing-session races (review 2026-10-14, H2): a session
        can be erased (GDPR `DELETE /agent/me`) or hard-deleted by
        retention WHILE a run is streaming. The FK then rejects the
        insert with `ForeignKeyViolationError` — translated here to
        `ChatStoreUnavailable` so every call site's existing
        best-effort `except ChatStoreUnavailable` handles it uniformly
        (the session is unavailable; the stream continues).
        """
        session_id_str = str(session_id)

        # jsonb: serialise via asyncpg's set_encoder; raw JSON string is
        # accepted by the JSONB column cast.
        tool_calls_json = (
            _json.dumps(tool_calls) if tool_calls is not None else None
        )
        try:
            steps_json = (
                _json.dumps(steps, allow_nan=False)
                if steps is not None
                else None
            )
        except (TypeError, ValueError) as exc:
            log.warning(
                "chat_store record_message: steps payload is not JSON-safe; "
                "persisting steps=NULL (session_id=%s, err=%r)",
                session_id_str,
                exc,
            )
            steps_json = None

        async def _tx(
            conn: asyncpg.Connection, steps_payload: str | None
        ) -> int:
            try:
                async with conn.transaction():
                    message_id: int = await conn.fetchval(
                        "INSERT INTO chat_messages "
                        "(session_id, role, content, tool_calls, steps, "
                        "tokens, prompt_tokens, completion_tokens) "
                        "VALUES ($1, $2, $3, $4::jsonb, $5::jsonb, $6, $7, $8) "
                        "RETURNING id",
                        session_id_str,
                        role,
                        content,
                        tool_calls_json,
                        steps_payload,
                        tokens,
                        prompt_tokens,
                        completion_tokens,
                    )
                    await conn.execute(
                        "UPDATE chat_sessions SET last_active_at=now() "
                        "WHERE id=$1 AND user_id=$2 AND deleted_at IS NULL",
                        session_id_str,
                        user_id,
                    )
                return message_id
            except asyncpg.exceptions.ForeignKeyViolationError as exc:
                # The parent session vanished under us (erasure / retention
                # hard-delete). Same contract as a pool outage: the store
                # is unavailable for this write; the caller warn-logs and
                # the run continues. No retry — a deleted session never
                # comes back.
                raise ChatStoreUnavailable(
                    f"record_message failed: session {session_id_str} no "
                    "longer exists (erased or hard-deleted mid-run)"
                ) from exc

        try:
            return await self._with_retry(
                "record_message", lambda conn: _tx(conn, steps_json)
            )
        except asyncpg.exceptions.DataError as exc:
            if steps_json is None:
                raise
            log.warning(
                "chat_store record_message: Postgres rejected the steps "
                "jsonb; retrying without the trace (session_id=%s, err=%r)",
                session_id_str,
                exc,
            )
            return await self._with_retry(
                "record_message", lambda conn: _tx(conn, None)
            )

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

    async def set_title_if_empty(
        self,
        session_id: str | uuid_lib.UUID,
        user_id: int,
        title: str,
    ) -> bool:
        """Set the session's title only while it is still empty.

        The deterministic-title write of the first exchange
        (openspec/changes/agent-chat-fixes D2): the `title=''` predicate
        makes the write idempotent and never-clobbering, so a
        continuation or resumed run needs no extra branching — an
        already-titled session is left alone. All three guards ride the
        SQL: ownership (`user_id`), retention state (`deleted_at IS NULL`)
        and emptiness. Returns True when the row was updated, False when
        no row matched (unknown id, other user, soft-deleted, or a
        non-empty title already set).
        """

        async def _update(conn: asyncpg.Connection) -> bool:
            tag = await conn.execute(
                "UPDATE chat_sessions SET title=$3 "
                "WHERE id=$1 AND user_id=$2 AND title='' "
                "AND deleted_at IS NULL",
                str(session_id),
                user_id,
                title,
            )
            return _parse_command_tag(tag) == 1

        return await self._with_retry("set_title_if_empty", _update)

    async def set_title(
        self,
        session_id: str | uuid_lib.UUID,
        user_id: int,
        title: str,
    ) -> bool:
        """Overwrite the session's title, owner- and retention-guarded.

        The refinement write (openspec/changes/agent-chat-fixes D2): the
        async title-refinement task replaces the deterministic title in
        place. Unlike `set_title_if_empty` there is no emptiness
        predicate — the refinement overwrites whatever is there — but
        ownership and `deleted_at IS NULL` still gate the write, so a
        session soft-deleted before the task lands is not touched.
        Returns True when the row was updated, False when no row matched.
        """

        async def _update(conn: asyncpg.Connection) -> bool:
            tag = await conn.execute(
                "UPDATE chat_sessions SET title=$3 "
                "WHERE id=$1 AND user_id=$2 AND deleted_at IS NULL",
                str(session_id),
                user_id,
                title,
            )
            return _parse_command_tag(tag) == 1

        return await self._with_retry("set_title", _update)

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

    async def hard_delete_soft_deleted(self, grace_days: int) -> list[str]:
        """Delete sessions whose `deleted_at` is past the grace window.

        Second phase of the two-phase retention. Returns the ids of the
        hard-deleted sessions — the caller increments
        `agent_chat_retention_deleted_total` by `len(ids)` and hands the
        ids to the derived-memory cleanup
        (`PrefsStore.delete_prefs_for_sessions`): a session's preferences
        do not outlive it (openspec/changes/agent-user-preferences D6).

        `chat_messages` cascades via `ON DELETE CASCADE` on the FK, so
        one statement cleans both tables.
        """

        async def _delete(conn: asyncpg.Connection) -> list[str]:
            rows = await conn.fetch(
                "DELETE FROM chat_sessions "
                "WHERE deleted_at IS NOT NULL "
                "AND deleted_at < now() - ($1 * interval '1 day') "
                "RETURNING id",
                grace_days,
            )
            return [str(r["id"]) for r in rows]

        return await self._with_retry("hard_delete_soft_deleted", _delete)

    async def delete_all_for_user(self, user_id: int) -> dict[str, Any]:
        """Owner-scoped erasure: delete EVERYTHING this user owns here.

        The GDPR right-to-erasure path (openspec/changes/agent-gdpr-compliance
        D3/D7) — distinct from the retention path above:

          - Soft-deleted rows are included. Retention phase 1 is not a
            precondition for erasure: an erasure request deletes whatever
            exists for the subject, including rows the user (or the
            retention cron) already soft-deleted.
          - ALL preference rows are deleted, NULL-source included — the
            erasure path is what collects prefs the retention path
            deliberately leaves behind (`delete_orphaned` skips them).
          - Owner predicate on EVERY statement (review discipline: erasure
            must be the *least* likely code path to be missing a
            `user_id` filter). The message delete joins through the
            user's sessions — never delete-by-session-id alone.

        ONE transaction, statements in reverse-dependency order
        (messages → sessions → prefs): the explicit child-first delete
        gives an exact message count for the erase response/audit instead
        of relying on the FK CASCADE side effect, and the transaction
        keeps a concurrent retention purge from interleaving. Returns the
        per-table counts plus the deleted session ids
        (`{"messages": n, "sessions": n, "prefs": n, "session_ids": [...]}`)
        — the ids double as langGraph thread ids for continuation runs
        (whose graph threads are keyed by session, not by a run row), so
        the erase endpoint GCs them. A second call on an already-erased
        user returns zeros + an empty id list (idempotent).
        """

        async def _tx(conn: asyncpg.Connection) -> dict[str, Any]:
            async with conn.transaction():
                # (a) Messages of the user's sessions — soft-deleted
                # sessions included (their messages must go too).
                msg_rows = await conn.fetch(
                    "DELETE FROM chat_messages m "
                    "USING chat_sessions s "
                    "WHERE m.session_id = s.id AND s.user_id = $1 "
                    "RETURNING m.id",
                    user_id,
                )
                # (b) Every session row for the user, soft-deleted included.
                sess_rows = await conn.fetch(
                    "DELETE FROM chat_sessions "
                    "WHERE user_id = $1 "
                    "RETURNING id",
                    user_id,
                )
                # (c) Every pref row for the user — NULL-source included
                # (erasure path, not the retention path).
                pref_rows = await conn.fetch(
                    "DELETE FROM chat_user_prefs "
                    "WHERE user_id = $1 "
                    "RETURNING id",
                    user_id,
                )
            return {
                "messages": len(msg_rows),
                "sessions": len(sess_rows),
                "prefs": len(pref_rows),
                # The deleted session ids double as langGraph thread ids:
                # continuation runs bind their graph thread to the SESSION
                # id and carry no agent.runs row, so the erase path is the
                # only place those checkpoint threads can be GC'd from
                # (review 2026-10-14 compliance fix). String form for the
                # JSON serializer.
                "session_ids": [str(r["id"]) for r in sess_rows],
            }

        return await self._with_retry("delete_all_for_user", _tx)

    async def list_all_sessions_for_user(self, user_id: int) -> list[ChatSession]:
        """Export read: every session row for the user, soft-deleted INCLUDED.

        Unpaginated and unfiltered-by-state on purpose (export
        completeness — `openspec/changes/agent-gdpr-erasure` "sessions
        (every `agent.chat_sessions` row for the user, including
        soft-deleted ones)"); ordered `(started_at ASC, id ASC)` so the
        dump is deterministic. The `id` tiebreaker keeps same-microsecond
        rows in insertion order.
        """

        async def _select(conn: asyncpg.Connection) -> list[ChatSession]:
            rows = await conn.fetch(
                "SELECT id, user_id, title, started_at, last_active_at, "
                "ended_at, deleted_at FROM chat_sessions "
                "WHERE user_id=$1 "
                "ORDER BY started_at ASC, id ASC",
                user_id,
            )
            return [_row_to_session(r) for r in rows]

        return await self._with_retry("list_all_sessions_for_user", _select)

    async def list_all_messages_for_user(self, user_id: int) -> list[MessageRow]:
        """Export read: every message of the user's sessions, soft-deleted included.

        Joins through the user's sessions (including soft-deleted ones —
        export completeness) rather than taking session ids as an
        argument, so the owner predicate lives in the SQL. Ordered
        `(created_at ASC, id ASC)`, the same chronological order as
        `list_messages`; `tool_calls::text` / `steps::text` +
        `json.loads` decode the jsonb the same way.
        """

        async def _select(conn: asyncpg.Connection) -> list[MessageRow]:
            rows = await conn.fetch(
                "SELECT m.id, m.session_id, m.role, m.content, "
                "m.tool_calls::text AS tool_calls, m.steps::text AS steps, "
                "m.tokens, m.prompt_tokens, m.completion_tokens, m.created_at "
                "FROM chat_messages m "
                "JOIN chat_sessions s ON s.id = m.session_id "
                "WHERE s.user_id=$1 "
                "ORDER BY m.created_at ASC, m.id ASC",
                user_id,
            )
            return [_row_to_message(r) for r in rows]

        return await self._with_retry("list_all_messages_for_user", _select)


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
    raw_steps = row["steps"]
    return MessageRow(
        id=row["id"],
        role=row["role"],
        content=row["content"],
        tool_calls=(
            _json.loads(raw_tool_calls) if raw_tool_calls is not None else None
        ),
        tokens=row["tokens"],
        prompt_tokens=row["prompt_tokens"],
        completion_tokens=row["completion_tokens"],
        created_at=row["created_at"],
        session_id=str(row["session_id"]),
        steps=_json.loads(raw_steps) if raw_steps is not None else None,
    )


_EPOCH = datetime(1970, 1, 1, tzinfo=UTC)


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
