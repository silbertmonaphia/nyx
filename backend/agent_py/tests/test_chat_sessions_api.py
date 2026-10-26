"""HTTP-surface tests for the chat-sessions API (openspec/changes/agent-chat-history-ui).

Drives the REAL FastAPI app (router registered by `create_app`) over
httpx's ASGI transport, against a real Postgres pool — every assertion
goes through the full request → handler → SQL → serialized-JSON path
(the store is only used to seed data and to read values back for
comparison).

Covers (tasks.md 3.1):
  1. List: newest-first ordering + per-row `message_count` (zero-message
     sessions read 0)
  2. Pagination: `limit+1` probe, cursor round-trip through BOTH pages,
     `next_cursor=null` on the last page, wire-format of the cursor,
     same-`last_active_at` ties neither skipped nor duplicated
  3. Invalid `limit` / undecodable `before` → 400
  4. Cross-user isolation on the list (owner-scoped)
  5. Message replay: chronological order, `tool_calls` as a JSON array
  6. Soft-deleted sessions: absent from the list, 404 on replay + resume
  7. Create → fresh uuid4 + `started_at`, appears empty in the list
  8. Resume refreshes `last_active_at` and re-ranks the sidebar
  9. Delete → soft-delete (row + messages remain), hides from list /
     replay, second delete → 404, cross-user delete → 404
  10. Missing / non-integer `X-Nyx-User-Id` → 400; malformed path id → 404
  11. Per-request OTel span attributes (`endpoint`, `user_id`,
     `result_count`; none on 404)

Gated on `TEST_ASYNC_PG_DSN` (self-skips without a live Postgres).
"""
from __future__ import annotations

import asyncio
import base64
import os
import uuid as _uuid
from datetime import UTC, datetime

import asyncpg
import httpx
import pytest

from tests.service_auth_fixtures import SERVICE_AUTH_HEADERS

_DSN = os.environ.get("TEST_ASYNC_PG_DSN")

pytestmark = pytest.mark.skipif(
    not _DSN,
    reason="TEST_ASYNC_PG_DSN not set; needs a live Postgres",
)

_EPOCH = datetime(1970, 1, 1, tzinfo=UTC)


# ---------------------------------------------------------------------------
# Fixtures — same drop/rebuild + alembic pattern as test_chat_store.py
# ---------------------------------------------------------------------------


async def _drop_schemas() -> None:
    conn = await asyncpg.connect(dsn=_DSN)
    try:
        await conn.execute("DROP SCHEMA IF EXISTS langgraph CASCADE")
        await conn.execute("DROP SCHEMA IF EXISTS agent CASCADE")
    finally:
        await conn.close()


async def _build_pool() -> asyncpg.Pool:
    pool = asyncpg.create_pool(
        dsn=_DSN,
        min_size=1,
        max_size=10,
        server_settings={"search_path": "agent,langgraph"},
    )
    return await pool


@pytest.fixture
async def pool():
    """Isolated pool per test: drop and rebuild both schemas so the test
    owns a clean slate, then run alembic to `head`."""
    await _drop_schemas()
    pool = await _build_pool()
    from migrations._dev_upgrade_hook import run_upgrade_head

    await run_upgrade_head()
    yield pool
    await pool.close()
    await _drop_schemas()


@pytest.fixture
async def store(pool):
    """Seeding handle over the test pool (assertions go through HTTP)."""
    from agent.chat_store import ChatStore

    return ChatStore(pool)


def _build_app(pool: asyncpg.Pool, **settings_overrides):
    """Real app, no lifespan.

    The router is registered by `create_app`; the handlers read only
    `app.state.chat_store` + `app.state.settings`, so the test wires
    those directly instead of paying for redis / checkpointer / crons
    (the lifespan wiring itself is covered by test_sweeper.py).
    """
    from agent.chat_store import ChatStore
    from agent.config import Settings
    from agent.main import create_app

    settings = Settings(
        _env_file=None,  # hermetic: never read the developer's .env
        llm_base_url="http://localhost:9999/v1",
        **settings_overrides,
    )
    app = create_app(settings)
    app.state.settings = settings
    app.state.chat_store = ChatStore(pool)
    return app


@pytest.fixture
async def app(pool):
    return _build_app(pool)


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


async def _call(
    app,
    method: str,
    path: str,
    *,
    user_id: int | str | None = 1,
):
    """Drive the app over ASGI. `user_id=None` omits the header entirely."""
    headers: dict[str, str] = {}
    if user_id is not None:
        headers["X-Nyx-User-Id"] = str(user_id)
    transport = httpx.ASGITransport(app=app)
    async with httpx.AsyncClient(
        transport=transport, base_url="http://test", headers=SERVICE_AUTH_HEADERS
    ) as client:
        return await client.request(method, path, headers=headers)


async def _seed_session(
    store,
    pool,
    *,
    user_id: int,
    age_minutes: float = 0.0,
    messages=(),
) -> str:
    """Create a session, record `messages`, then age `last_active_at`.

    Aging last matters: `record_message` bumps `last_active_at` to
    `now()`, so an aged-then-messaged session would jump back to the top.
    """
    sid = str(_uuid.uuid4())
    await store.create_session(user_id, sid)
    for msg in messages:
        await store.record_message(sid, user_id, **msg)
    if age_minutes:
        async with pool.acquire() as conn:
            await conn.execute(
                "UPDATE chat_sessions "
                "SET last_active_at = now() - ($2 * interval '1 minute') "
                "WHERE id=$1",
                sid,
                age_minutes,
            )
    return sid


def _b64(raw: str) -> str:
    """base64url-encode the cursor payload with padding stripped."""
    return base64.urlsafe_b64encode(raw.encode("ascii")).decode("ascii").rstrip("=")


# ---------------------------------------------------------------------------
# List — ordering + message_count
# ---------------------------------------------------------------------------


async def test_list_newest_first_with_message_count(app, pool, store):
    old = await _seed_session(
        store,
        pool,
        user_id=1,
        age_minutes=120,
        messages=[{"role": "user", "content": "bye"}],
    )
    mid = await _seed_session(store, pool, user_id=1, age_minutes=60)
    new = await _seed_session(
        store,
        pool,
        user_id=1,
        messages=[
            {"role": "user", "content": "hi"},
            {"role": "assistant", "content": "hello", "tokens": 3},
        ],
    )

    resp = await _call(app, "GET", "/agent/sessions")
    assert resp.status_code == 200, resp.text
    body = resp.json()

    # Reverse chronological, no tie on last_active_at.
    assert [s["id"] for s in body["sessions"]] == [new, mid, old]
    # count(m.id), not count(*) — the zero-message session reads 0.
    assert {s["id"]: s["message_count"] for s in body["sessions"]} == {
        new: 2,
        mid: 0,
        old: 1,
    }
    assert body["next_cursor"] is None

    first = body["sessions"][0]
    assert first["user_id"] == 1
    assert first["title"] == ""
    assert first["ended_at"] is None
    # Wire-clean timestamps: ISO-8601 strings with a UTC offset.
    assert datetime.fromisoformat(first["started_at"]).tzinfo is not None
    assert datetime.fromisoformat(first["last_active_at"]).tzinfo is not None


async def test_cross_user_list_is_owner_scoped(app, pool, store):
    mine = await _seed_session(store, pool, user_id=1)
    theirs = await _seed_session(store, pool, user_id=2)

    resp = await _call(app, "GET", "/agent/sessions", user_id=1)
    assert resp.status_code == 200, resp.text
    ids = [s["id"] for s in resp.json()["sessions"]]
    assert ids == [mine]
    assert theirs not in ids


async def test_list_page_is_a_single_statement(pool, store):
    """N+1 guard on `message_count`: the whole page is ONE statement.

    The count rides the same `LEFT JOIN ... GROUP BY` as the page rows —
    a per-row count query would show up as N+1 statements here.
    """
    for age in (30, 20, 10):
        await _seed_session(store, pool, user_id=1, age_minutes=age)

    # max_size=1 pins every acquire to the same connection, so a query
    # logger attached to it sees every statement the handler issues.
    single = await asyncpg.create_pool(
        dsn=_DSN,
        min_size=1,
        max_size=1,
        server_settings={"search_path": "agent,langgraph"},
    )
    try:
        conn = await single.acquire()
        recorded: list[str] = []
        conn.add_query_logger(lambda record: recorded.append(record.query))
        await single.release(conn)

        resp = await _call(_build_app(single), "GET", "/agent/sessions")
        assert resp.status_code == 200, resp.text
        assert len(resp.json()["sessions"]) == 3
        # Query loggers fire via `loop.call_soon` — give them a tick.
        await asyncio.sleep(0.01)

        page_queries = [q for q in recorded if "FROM chat_sessions" in q]
        assert len(page_queries) == 1, (
            f"the page must be one statement (no N+1); saw {recorded}"
        )
        assert "count(m.id)" in page_queries[0]
        assert "LEFT JOIN chat_messages" in page_queries[0]
    finally:
        await single.close()


# ---------------------------------------------------------------------------
# Pagination
# ---------------------------------------------------------------------------


async def test_pagination_cursor_walks_pages(app, pool, store):
    s1 = await _seed_session(store, pool, user_id=1, age_minutes=180)
    s2 = await _seed_session(store, pool, user_id=1, age_minutes=120)
    s3 = await _seed_session(store, pool, user_id=1, age_minutes=60)

    page1 = (await _call(app, "GET", "/agent/sessions?limit=2")).json()
    assert [s["id"] for s in page1["sessions"]] == [s3, s2]
    assert page1["next_cursor"] is not None

    # Wire format: base64url of `<unix-microseconds>_<uuid>` naming the
    # LAST INCLUDED row (s2) — the spec's cursor contract, checked
    # against the row's actual `last_active_at`.
    raw = base64.urlsafe_b64decode(
        page1["next_cursor"] + "=" * (-len(page1["next_cursor"]) % 4)
    ).decode("ascii")
    micros_str, sep, cursor_sid = raw.partition("_")
    assert sep == "_"
    assert cursor_sid == s2
    async with pool.acquire() as conn:
        row = await conn.fetchrow(
            "SELECT last_active_at FROM agent.chat_sessions WHERE id=$1", s2
        )
    assert abs(int(micros_str) / 1e6 - row["last_active_at"].timestamp()) < 1e-6, (
        "cursor microseconds must name the boundary row exactly"
    )

    page2 = (
        await _call(app, "GET", f"/agent/sessions?limit=2&before={page1['next_cursor']}")
    ).json()
    assert [s["id"] for s in page2["sessions"]] == [s1]
    assert page2["next_cursor"] is None, "last page must not offer a cursor"


async def test_pagination_stable_across_same_last_active(app, pool, store):
    """Two sessions pinned to the same `last_active_at`: the `id DESC`
    tiebreaker must carry the order — page pairs stay disjoint + complete."""
    a = await _seed_session(store, pool, user_id=1)
    b = await _seed_session(store, pool, user_id=1)
    # One statement → one `now()` → one shared timestamp for both rows.
    async with pool.acquire() as conn:
        await conn.execute(
            "UPDATE chat_sessions SET last_active_at = now() WHERE id IN ($1, $2)",
            a,
            b,
        )

    hi, lo = sorted([a, b], reverse=True)  # uuid text order == uuid order

    page1 = (await _call(app, "GET", "/agent/sessions?limit=1")).json()
    assert [s["id"] for s in page1["sessions"]] == [hi]
    assert page1["next_cursor"] is not None

    page2 = (
        await _call(app, "GET", f"/agent/sessions?limit=1&before={page1['next_cursor']}")
    ).json()
    assert [s["id"] for s in page2["sessions"]] == [lo]
    assert page2["next_cursor"] is None


async def test_valid_cursor_past_the_end_returns_empty_page(app, pool, store):
    await _seed_session(store, pool, user_id=1)
    # Cursor naming (epoch, some uuid) — every real row sorts above it.
    cursor = _b64(f"{0}_{_uuid.uuid4()}")
    resp = await _call(app, "GET", f"/agent/sessions?before={cursor}")
    assert resp.status_code == 200, resp.text
    assert resp.json() == {"sessions": [], "next_cursor": None}


async def test_default_limit_comes_from_settings(pool, store):
    app = _build_app(pool, sessions_page_size=2)
    for age in (30, 20, 10):
        await _seed_session(store, pool, user_id=1, age_minutes=age)

    body = (await _call(app, "GET", "/agent/sessions")).json()
    assert len(body["sessions"]) == 2
    assert body["next_cursor"] is not None


@pytest.mark.parametrize("query", ["limit=0", "limit=51", "limit=-1", "limit=abc", "limit=1.5", "limit="])
async def test_invalid_limit_400(app, query):
    resp = await _call(app, "GET", f"/agent/sessions?{query}")
    assert resp.status_code == 400, resp.text


@pytest.mark.parametrize(
    "cursor",
    [
        "not-base64!!!",
        _b64("no-underscore"),
        _b64("123_not-a-uuid"),
        _b64(f"{0}_"),  # empty id
        _b64("x_" + str(_uuid.uuid4())),  # non-integer micros
        _b64(f"{10**30}_{_uuid.uuid4()}"),  # micros outside the calendar
        "",  # explicitly empty
    ],
)
async def test_bad_cursor_400(app, cursor):
    resp = await _call(app, "GET", f"/agent/sessions?before={cursor}")
    assert resp.status_code == 400, resp.text


# ---------------------------------------------------------------------------
# Titles
# ---------------------------------------------------------------------------


async def test_list_surfaces_a_stored_title_and_create_starts_empty(
    app, pool, store
):
    """The title written by the title pipeline (agent-chat-fixes 2.2/2.3)
    reaches the sidebar wire as-is; a freshly created session still starts
    with `title=''` — the derivation happens on the first turn, not at
    creation."""
    sid = await _seed_session(store, pool, user_id=1)
    assert await store.set_title_if_empty(sid, 1, "熬夜加班") is True

    listed = (await _call(app, "GET", "/agent/sessions")).json()
    row = next(s for s in listed["sessions"] if s["id"] == sid)
    assert row["title"] == "熬夜加班"

    created = (await _call(app, "POST", "/agent/sessions")).json()
    fresh = (await _call(app, "GET", "/agent/sessions")).json()["sessions"]
    created_row = next(s for s in fresh if s["id"] == created["session_id"])
    assert created_row["title"] == ""

    # The title is a session attribute, not a message: the replay surface
    # is unchanged by it.
    replay = await _call(app, "GET", f"/agent/sessions/{sid}/messages")
    assert replay.status_code == 200
    assert replay.json()["messages"] == []


# ---------------------------------------------------------------------------
# Messages — replay
# ---------------------------------------------------------------------------


async def test_messages_replay_order_and_tool_calls_array(app, pool, store):
    tool_calls = [
        {
            "step_id": "step_1",
            "call_id": "call_1",
            "name": "list_feeds",
            "args": {"limit": 10},
        }
    ]
    steps = [
        {
            "id": "step_1",
            "kind": "tool",
            "status": "ok",
            "tool_name": "list_feeds",
            "tool_args": {"limit": 10},
            "tool_result": {"items": []},
            "retry_count": 0,
        },
        {"id": "step_2", "kind": "llm", "status": "ok"},
    ]
    sid = await _seed_session(
        store,
        pool,
        user_id=1,
        messages=[
            {"role": "user", "content": "list my feeds"},
            {
                "role": "assistant",
                "content": "here you go",
                "tool_calls": tool_calls,
                "steps": steps,
                "tokens": 42,
                "prompt_tokens": 30,
                "completion_tokens": 12,
            },
            {"role": "user", "content": "thanks"},
        ],
    )

    resp = await _call(app, "GET", f"/agent/sessions/{sid}/messages")
    assert resp.status_code == 200, resp.text
    messages = resp.json()["messages"]

    assert [m["role"] for m in messages] == ["user", "assistant", "user"]
    assert [m["content"] for m in messages] == [
        "list my feeds",
        "here you go",
        "thanks",
    ]
    # Chronological, id as the tiebreaker.
    ids = [m["id"] for m in messages]
    assert ids == sorted(ids)
    # The wire contract: a JSON array, not a JSON-encoded string.
    assert isinstance(messages[1]["tool_calls"], list)
    assert messages[1]["tool_calls"] == tool_calls
    assert isinstance(messages[1]["steps"], list)
    assert messages[1]["steps"] == steps
    assert messages[1]["tokens"] == 42
    assert messages[1]["prompt_tokens"] == 30
    assert messages[1]["completion_tokens"] == 12
    assert messages[0]["tool_calls"] is None
    assert messages[0]["steps"] is None
    assert messages[2]["steps"] is None
    assert messages[0]["prompt_tokens"] is None
    assert messages[0]["completion_tokens"] is None
    assert datetime.fromisoformat(messages[0]["created_at"]).tzinfo is not None


async def test_messages_replay_preserves_native_rag_and_llm_steps(
    app, pool, store
) -> None:
    """A `[rag, llm]` plan dispatches no tools: `tool_calls` is NULL while
    the persisted trace still replays both steps (spec scenario)."""
    steps = [
        {"id": "step_1", "kind": "rag", "status": "ok"},
        {"id": "step_2", "kind": "llm", "status": "ok"},
    ]
    sid = await _seed_session(
        store,
        pool,
        user_id=1,
        messages=[
            {"role": "user", "content": "summarize"},
            {"role": "assistant", "content": "summary", "steps": steps},
        ],
    )

    resp = await _call(app, "GET", f"/agent/sessions/{sid}/messages")
    assert resp.status_code == 200, resp.text
    messages = resp.json()["messages"]

    assert messages[1]["tool_calls"] is None
    assert messages[1]["steps"] == steps


async def test_cross_user_messages_404(app, pool, store):
    theirs = await _seed_session(
        store, pool, user_id=2, messages=[{"role": "user", "content": "secret"}]
    )

    resp = await _call(app, "GET", f"/agent/sessions/{theirs}/messages", user_id=1)
    assert resp.status_code == 404, resp.text

    # Control: the owner still replays the same session.
    own = await _call(app, "GET", f"/agent/sessions/{theirs}/messages", user_id=2)
    assert own.status_code == 200
    assert own.json()["messages"][0]["content"] == "secret"


async def test_soft_deleted_session_messages_404(app, pool, store):
    """A retention-cron soft-delete (no user request involved) hides the
    session from the read surface immediately."""
    sid = await _seed_session(
        store, pool, user_id=1, messages=[{"role": "user", "content": "kept"}]
    )
    assert await store.soft_delete_session(sid, 1) is True

    resp = await _call(app, "GET", f"/agent/sessions/{sid}/messages")
    assert resp.status_code == 404, resp.text
    # The rows still exist — soft-delete is state, not deletion.
    async with pool.acquire() as conn:
        assert await conn.fetchval(
            "SELECT count(*) FROM agent.chat_messages WHERE session_id=$1", sid
        ) == 1


# ---------------------------------------------------------------------------
# Create / resume
# ---------------------------------------------------------------------------


async def test_create_returns_uuid4_and_appears_empty_in_list(app):
    resp = await _call(app, "POST", "/agent/sessions")
    assert resp.status_code == 200, resp.text
    body = resp.json()

    session_id = body["session_id"]
    assert _uuid.UUID(session_id).version == 4
    assert datetime.fromisoformat(body["started_at"]).tzinfo is not None

    listed = (await _call(app, "GET", "/agent/sessions")).json()
    row = next(s for s in listed["sessions"] if s["id"] == session_id)
    assert row["message_count"] == 0
    assert row["title"] == ""
    assert row["last_active_at"] == row["started_at"]


async def test_resume_touches_last_active_and_reranks(app, pool, store):
    resumed = await _seed_session(store, pool, user_id=1, age_minutes=45)
    await _seed_session(store, pool, user_id=1, age_minutes=5)  # currently first

    before = await store.get_session(resumed, 1)
    assert before is not None
    await asyncio.sleep(0.005)

    resp = await _call(app, "POST", f"/agent/sessions/{resumed}/resume")
    assert resp.status_code == 200, resp.text
    body = resp.json()
    assert body["session_id"] == resumed
    assert datetime.fromisoformat(body["resumed_at"]).tzinfo is not None

    after = await store.get_session(resumed, 1)
    assert after is not None
    assert after.last_active_at > before.last_active_at

    listed = (await _call(app, "GET", "/agent/sessions")).json()
    assert listed["sessions"][0]["id"] == resumed, (
        "a resumed session must rank at the top of the sidebar"
    )


async def test_resume_unknown_foreign_or_deleted_404(app, pool, store):
    unknown = _uuid.uuid4()
    assert (
        await _call(app, "POST", f"/agent/sessions/{unknown}/resume")
    ).status_code == 404

    theirs = await _seed_session(store, pool, user_id=2)
    assert (
        await _call(app, "POST", f"/agent/sessions/{theirs}/resume", user_id=1)
    ).status_code == 404

    deleted = await _seed_session(store, pool, user_id=1)
    assert await store.soft_delete_session(deleted, 1) is True
    assert (
        await _call(app, "POST", f"/agent/sessions/{deleted}/resume")
    ).status_code == 404


# ---------------------------------------------------------------------------
# Delete
# ---------------------------------------------------------------------------


async def test_delete_soft_deletes_and_hides_immediately(app, pool, store):
    sid = await _seed_session(
        store, pool, user_id=1, messages=[{"role": "user", "content": "keep"}]
    )

    resp = await _call(app, "DELETE", f"/agent/sessions/{sid}")
    assert resp.status_code == 200, resp.text
    assert resp.json()["session_id"] == sid

    listed = (await _call(app, "GET", "/agent/sessions")).json()
    assert sid not in [s["id"] for s in listed["sessions"]]
    assert (
        await _call(app, "GET", f"/agent/sessions/{sid}/messages")
    ).status_code == 404
    # Second delete: the row is invisible, so 404 (idempotency is expressed
    # as "already gone", not as a silent 200).
    assert (await _call(app, "DELETE", f"/agent/sessions/{sid}")).status_code == 404

    # Soft-delete (not hard): the row and its messages survive until the
    # retention cron reaps them.
    async with pool.acquire() as conn:
        deleted_at = await conn.fetchval(
            "SELECT deleted_at FROM agent.chat_sessions WHERE id=$1", sid
        )
        msg_count = await conn.fetchval(
            "SELECT count(*) FROM agent.chat_messages WHERE session_id=$1", sid
        )
    assert deleted_at is not None
    assert msg_count == 1


async def test_delete_cross_user_404_and_leaves_row_untouched(app, pool, store):
    theirs = await _seed_session(store, pool, user_id=2)

    resp = await _call(app, "DELETE", f"/agent/sessions/{theirs}", user_id=1)
    assert resp.status_code == 404, resp.text

    async with pool.acquire() as conn:
        deleted_at = await conn.fetchval(
            "SELECT deleted_at FROM agent.chat_sessions WHERE id=$1", theirs
        )
    assert deleted_at is None, "a foreign delete must not touch the row"


async def test_malformed_session_id_is_404_not_500(app):
    for method, path in (
        ("GET", "/agent/sessions/not-a-uuid/messages"),
        ("POST", "/agent/sessions/not-a-uuid/resume"),
        ("DELETE", "/agent/sessions/not-a-uuid"),
    ):
        resp = await _call(app, method, path)
        assert resp.status_code == 404, f"{method} {path}: {resp.text}"


# ---------------------------------------------------------------------------
# Header enforcement
# ---------------------------------------------------------------------------


async def test_missing_or_non_integer_user_header_400(app):
    resp = await _call(app, "GET", "/agent/sessions", user_id=None)
    assert resp.status_code == 400
    assert "missing X-Nyx-User-Id" in resp.json()["detail"]

    resp = await _call(app, "GET", "/agent/sessions", user_id="abc")
    assert resp.status_code == 400
    assert "must be an integer" in resp.json()["detail"]

    sid = _uuid.uuid4()
    assert (
        await _call(app, "GET", f"/agent/sessions/{sid}/messages", user_id=None)
    ).status_code == 400
    assert (await _call(app, "POST", "/agent/sessions", user_id=None)).status_code == 400
    assert (
        await _call(app, "DELETE", f"/agent/sessions/{sid}", user_id=None)
    ).status_code == 400
    assert (
        await _call(app, "POST", f"/agent/sessions/{sid}/resume", user_id=None)
    ).status_code == 400


# ---------------------------------------------------------------------------
# OTel spans
# ---------------------------------------------------------------------------


def _patch_tracer(monkeypatch):
    """Point the router's module tracer at an in-memory exporter.

    Patched rather than set globally: the module-level
    `trace.get_tracer(...)` is a proxy resolved against the process
    provider, and test ordering would decide whether that is the real
    provider or the no-op default.
    """
    from opentelemetry.sdk.resources import Resource
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import SimpleSpanProcessor
    from opentelemetry.sdk.trace.export.in_memory_span_exporter import (
        InMemorySpanExporter,
    )

    import agent.api.chat_sessions as chat_sessions_module

    exporter = InMemorySpanExporter()
    provider = TracerProvider(resource=Resource.create({}))
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    monkeypatch.setattr(
        chat_sessions_module, "_tracer", provider.get_tracer("test-chat-sessions")
    )
    return exporter


async def test_list_span_attributes(app, pool, store, monkeypatch):
    """Spec scenario "Per-request span attributes are populated"."""
    exporter = _patch_tracer(monkeypatch)

    for age in (30, 20, 10):
        await _seed_session(store, pool, user_id=7, age_minutes=age)

    resp = await _call(app, "GET", "/agent/sessions", user_id=7)
    assert resp.status_code == 200, resp.text

    spans = [s for s in exporter.get_finished_spans() if s.name == "agent.sessions.list"]
    assert len(spans) == 1
    attrs = dict(spans[0].attributes)
    assert attrs["endpoint"] == "list_sessions"
    assert attrs["user_id"] == 7
    assert attrs["result_count"] == 3


async def test_404_span_has_no_result_count(app, monkeypatch):
    exporter = _patch_tracer(monkeypatch)

    resp = await _call(app, "GET", f"/agent/sessions/{_uuid.uuid4()}/messages")
    assert resp.status_code == 404

    spans = [
        s for s in exporter.get_finished_spans() if s.name == "agent.sessions.messages"
    ]
    assert len(spans) == 1
    attrs = dict(spans[0].attributes)
    assert attrs["endpoint"] == "session_messages"
    assert "user_id" in attrs
    assert "result_count" not in attrs
