"""REST surface over chat sessions (openspec/changes/agent-chat-history-ui).

Five owner-scoped endpoints, mirroring the Go `/api/chat/sessions` proxy
routes 1:1:

  - `GET    /agent/sessions`                       — paginated session list
  - `GET    /agent/sessions/{session_id}/messages` — message replay
  - `POST   /agent/sessions`                       — create an empty session
  - `POST   /agent/sessions/{session_id}/resume`   — refresh `last_active_at`
  - `DELETE /agent/sessions/{session_id}`          — soft-delete

Trust model: identical to `/agent/run` (see `main.py`). The Go chat
handler verifies the user JWT and injects the verified id as
`X-Nyx-User-Id`; this service never sees the token. A missing or
non-integer header is a 400 (fail loudly — never run as an implicit
user 0). Every store call carries that user id, and cross-user /
unknown / soft-deleted ids all collapse to 404 — never 403 — so the
surface cannot leak the existence of another user's session ids
(design.md risk "Cross-user leak via 404 vs 403"). A path id that is
not a UUID is treated as "not found" (404) so asyncpg never sees an
uncastable parameter.

Pagination cursor: base64url (padding stripped) of
`<unix-microseconds>_<uuid>` — the `(last_active_at, id)` tuple of the
last row in the page (design.md D1). The `id` tiebreaker keeps the
total order strict when sessions share a `last_active_at` microsecond.
A cursor that fails to decode is a 400; the SPA recovers by re-fetching
page one (design.md risk "Pagination cursor drift"). The messages
replay uses a second, simpler cursor: a decimal string of the window's
oldest message id (`next_before`), valid only together with `limit`.

Serialization is explicit: asyncpg hands back `uuid.UUID` / `datetime`
objects, so every response field is converted to `str` / ISO-8601 here
(the Go proxy forwards the body verbatim; nothing downstream
re-serializes). `tool_calls` comes out of the store already decoded to
a list (or None) — never a JSON string (see `ChatStore.list_messages`).

Each endpoint opens one OTel span on the shared `nyx.agent` tracer
(`agent.sessions.{list,messages,create,resume,delete}`) with
`endpoint` + `user_id` attributes and `result_count` on the read paths
that produce a list (never set on 400/404 — the span then records only
endpoint / user_id).
"""

from __future__ import annotations

import base64
import uuid as uuid_lib
from datetime import UTC, datetime, timedelta

from fastapi import APIRouter, HTTPException, Request
from opentelemetry import trace

from ..chat_store import ChatStore, MessageRow, SessionSummary
from ..config import Settings, get_settings
from .deps import require_user_id as _deps_require_user_id

router = APIRouter()

# Same tracer name as the graph nodes, retention cron and sweeper; the
# export destination is owned by `setup_observability` in the lifespan.
_tracer = trace.get_tracer("nyx.agent")

_EPOCH = datetime(1970, 1, 1, tzinfo=UTC)


@router.get("/agent/sessions")
async def list_sessions(request: Request) -> dict:
    """One page of the caller's sessions, newest-active first.

    Response: `{"sessions": [...], "next_cursor": <cursor|null>}` — each
    session carries `message_count` (single-round-trip count, no N+1).
    """
    with _tracer.start_as_current_span("agent.sessions.list") as span:
        span.set_attribute("endpoint", "list_sessions")
        user_id = _require_user_id(request)
        span.set_attribute("user_id", user_id)
        store = _chat_store(request)
        limit = _parse_limit(request, _settings(request).sessions_page_size)
        before = _parse_before(request)
        sessions, next_cursor = await store.list_sessions(
            user_id, limit=limit, before=before
        )
        span.set_attribute("result_count", len(sessions))
        return {
            "sessions": [_session_json(s) for s in sessions],
            "next_cursor": next_cursor,
        }


@router.get("/agent/sessions/{session_id}/messages")
async def session_messages(request: Request, session_id: str) -> dict:
    """Chronological replay of one session's messages, optionally paged.

    Without pagination params the response is the full replay,
    `{"messages": [...]}` ordered `created_at ASC, id ASC` — the
    pre-pagination contract, byte-shape unchanged. With `limit` (and
    optionally `before`) the response is the newest window plus
    `has_more` / `next_before`
    (openspec/changes/agent-chat-history-perf). 404 when the session is
    unknown, foreign, or soft-deleted (the messages still exist in the
    table in the last case — they are just not exposed).
    """
    with _tracer.start_as_current_span("agent.sessions.messages") as span:
        span.set_attribute("endpoint", "session_messages")
        user_id = _require_user_id(request)
        span.set_attribute("user_id", user_id)
        session_id = _require_session_id(session_id)
        limit = _parse_message_limit(request)
        before = _parse_message_before(request, require_limit=limit is None)
        store = _chat_store(request)
        if limit is None:
            messages = await store.list_messages(session_id, user_id)
            if messages is None:
                raise HTTPException(status_code=404, detail="session not found")
            span.set_attribute("result_count", len(messages))
            return {"messages": [_message_json(m) for m in messages]}
        page = await store.list_messages_page(
            session_id, user_id, limit=limit, before=before
        )
        if page is None:
            raise HTTPException(status_code=404, detail="session not found")
        rows, has_more, next_before = page
        span.set_attribute("result_count", len(rows))
        return {
            "messages": [_message_json(m) for m in rows],
            "has_more": has_more,
            "next_before": next_before,
        }


@router.post("/agent/sessions")
async def create_session(request: Request) -> dict:
    """Create an empty session (the `+ New chat` button).

    Response: `{"session_id": <uuid4>, "started_at": <iso8601>}`. The
    session starts with zero messages; the next `/agent/run` echoing the
    id populates it.
    """
    with _tracer.start_as_current_span("agent.sessions.create") as span:
        span.set_attribute("endpoint", "create_session")
        user_id = _require_user_id(request)
        span.set_attribute("user_id", user_id)
        store = _chat_store(request)
        session_id = str(uuid_lib.uuid4())
        # `create_session` is idempotent on the id; a fresh uuid4 cannot
        # collide, so this is a plain INSERT. `title=""` per the spec —
        # the sidebar derives a placeholder from the first user message.
        await store.create_session(user_id, session_id, title="")
        created = await store.get_session(session_id, user_id)
        if created is None:
            # Unreachable short of a concurrent hard-delete between the
            # INSERT and the read; static detail, no internals on the wire.
            raise HTTPException(status_code=500, detail="session read-back failed")
        return {
            "session_id": session_id,
            "started_at": created.started_at.isoformat(),
        }


@router.post("/agent/sessions/{session_id}/resume")
async def resume_session(request: Request, session_id: str) -> dict:
    """Mark a session as active again (touches `last_active_at`).

    Response: `{"session_id": <uuid>, "resumed_at": <iso8601>}`. The
    touch refreshes the retention clock and re-ranks the session to the
    top of the sidebar. 404 for unknown / foreign / soft-deleted ids.
    """
    with _tracer.start_as_current_span("agent.sessions.resume") as span:
        span.set_attribute("endpoint", "resume_session")
        user_id = _require_user_id(request)
        span.set_attribute("user_id", user_id)
        session_id = _require_session_id(session_id)
        store = _chat_store(request)
        touched = await store.touch_session(session_id, user_id)
        if not touched:
            raise HTTPException(status_code=404, detail="session not found")
        return {
            "session_id": session_id,
            "resumed_at": datetime.now(UTC).isoformat(),
        }


@router.delete("/agent/sessions/{session_id}")
async def delete_session(request: Request, session_id: str) -> dict:
    """User-initiated soft-delete (`deleted_at = now()`).

    Response: `{"session_id": <uuid>, "deleted": true}`. Messages stay
    in the table until the retention cron hard-deletes the row (FK
    `ON DELETE CASCADE`); every read path hides the session immediately.
    A second DELETE on the same id is a 404 — the row is invisible.
    """
    with _tracer.start_as_current_span("agent.sessions.delete") as span:
        span.set_attribute("endpoint", "delete_session")
        user_id = _require_user_id(request)
        span.set_attribute("user_id", user_id)
        session_id = _require_session_id(session_id)
        store = _chat_store(request)
        deleted = await store.soft_delete_session(session_id, user_id)
        if not deleted:
            raise HTTPException(status_code=404, detail="session not found")
        return {"session_id": session_id, "deleted": True}


# ---------------------------------------------------------------------------
# Request helpers
# ---------------------------------------------------------------------------


def _require_user_id(request: Request) -> int:
    """Read the Go-verified user id from `X-Nyx-User-Id`.

    Shared implementation (`api/deps.require_user_id`, review N4
    2026-10-14): loud 400 for a misconfigured caller — never an
    implicit user 0.
    """
    return _deps_require_user_id(request)


def _require_session_id(raw: str) -> str:
    """Validate a path session id as a UUID; a malformed id is a 404.

    Treating garbage as "not found" (rather than 422/400) keeps the
    surface uniform — the id namespace is opaque to clients, and a
    non-UUID can never name an existing session.
    """
    try:
        return str(uuid_lib.UUID(raw))
    except (ValueError, AttributeError, TypeError) as exc:
        raise HTTPException(status_code=404, detail="session not found") from exc


def _chat_store(request: Request) -> ChatStore:
    """The `ChatStore` wired by the lifespan; absent when no DB is configured."""
    store = getattr(request.app.state, "chat_store", None)
    if store is None:
        raise HTTPException(status_code=503, detail="chat store unavailable")
    return store


def _settings(request: Request) -> Settings:
    """App-scoped settings (set by the lifespan); process singleton as fallback."""
    settings = getattr(request.app.state, "settings", None)
    return settings if settings is not None else get_settings()


def _parse_limit(request: Request, default: int) -> int:
    """`limit` query param: integer in `[1, 50]`, defaulting to settings."""
    raw = request.query_params.get("limit")
    if raw is None:
        return default
    try:
        limit = int(raw)
    except ValueError as exc:
        raise HTTPException(
            status_code=400, detail=f"limit must be an integer, got {raw!r}"
        ) from exc
    if not 1 <= limit <= 50:
        raise HTTPException(
            status_code=400, detail=f"limit must be in [1, 50]; got {limit}"
        )
    return limit


def _parse_before(request: Request) -> tuple[datetime, str] | None:
    """`before` query param: absent → first page, anything else must decode."""
    raw = request.query_params.get("before")
    if raw is None:
        return None
    return _decode_cursor(raw)


def _decode_cursor(cursor: str) -> tuple[datetime, str]:
    """Decode `before` into the `(last_active_at, id)` tuple it names.

    Wire format: base64url of `<unix-microseconds>_<uuid>` (see module
    docstring). Padding is optional (the encoder strips it; a re-padded
    token also decodes). Any base64 / shape / integer / uuid violation
    maps to 400 — the spec treats an unknown cursor as a client error
    the SPA recovers from by re-fetching page one.
    """
    try:
        padded = cursor + "=" * (-len(cursor) % 4)
        text = base64.b64decode(padded, altchars=b"-_", validate=True).decode("ascii")
        micros_str, sep, session_id = text.partition("_")
        if not sep or not micros_str or not session_id:
            raise ValueError(f"malformed cursor payload {text!r}")
        micros = int(micros_str)
        uuid_lib.UUID(session_id)
        # Exact inverse of `ChatStore._encode_cursor`'s integer
        # arithmetic — no float round-trip (see that docstring for why
        # 1µs matters). An absurd microsecond count overflows the
        # datetime range rather than the int, hence OverflowError.
        last_active_at = _EPOCH + timedelta(microseconds=micros)
    except (ValueError, OverflowError) as exc:
        # ValueError covers binascii.Error / UnicodeDecodeError / int() /
        # UUID(); OverflowError covers micros far outside the calendar.
        raise HTTPException(status_code=400, detail="invalid before cursor") from exc
    return last_active_at, session_id


def _parse_message_limit(request: Request) -> int | None:
    """`limit` query param for the messages replay: absent → full replay.

    Present it must be an integer in `[1, 200]` (else 400) — the replay
    window is client-chosen per request, so unlike the session list
    there is no settings default: absent means the legacy unbounded
    read, present means paginate.
    """
    raw = request.query_params.get("limit")
    if raw is None:
        return None
    try:
        limit = int(raw)
    except ValueError as exc:
        raise HTTPException(
            status_code=400, detail=f"limit must be an integer, got {raw!r}"
        ) from exc
    if not 1 <= limit <= 200:
        raise HTTPException(
            status_code=400, detail=f"limit must be in [1, 200]; got {limit}"
        )
    return limit


def _parse_message_before(request: Request, *, require_limit: bool) -> int | None:
    """`before` query param for the messages replay: a message-id cursor.

    The wire value is the decimal string this endpoint's responses mint
    (`next_before` — the oldest id of the previous window); clients
    MUST NOT construct cursor values themselves, so anything that is
    not a decimal string of a positive integer is a 400. `before`
    without `limit` is also a 400 — the two params go together, and
    silently ignoring the cursor would page the client's request
    wrong.
    """
    raw = request.query_params.get("before")
    if raw is None:
        return None
    if require_limit:
        raise HTTPException(
            status_code=400, detail="before requires limit"
        )
    try:
        value = int(raw) if raw.isascii() and raw.isdigit() else 0
    except ValueError as exc:
        raise HTTPException(
            status_code=400, detail="invalid before cursor"
        ) from exc
    if not 1 <= value <= 2**63 - 1:
        raise HTTPException(
            status_code=400, detail="invalid before cursor"
        )
    return value


# ---------------------------------------------------------------------------
# Serialization — asyncpg objects → JSON-safe primitives
# ---------------------------------------------------------------------------


def _session_json(session: SessionSummary) -> dict:
    return {
        "id": str(session.id),
        "user_id": session.user_id,
        "title": session.title,
        "started_at": session.started_at.isoformat(),
        "last_active_at": session.last_active_at.isoformat(),
        "ended_at": session.ended_at.isoformat() if session.ended_at else None,
        "message_count": session.message_count,
    }


def _message_json(message: MessageRow) -> dict:
    return {
        "id": message.id,
        "role": message.role,
        "content": message.content,
        "tool_calls": message.tool_calls,  # list | None, never a JSON string
        "steps": message.steps,
        "tokens": message.tokens,
        "prompt_tokens": message.prompt_tokens,
        "completion_tokens": message.completion_tokens,
        "created_at": message.created_at.isoformat(),
    }
