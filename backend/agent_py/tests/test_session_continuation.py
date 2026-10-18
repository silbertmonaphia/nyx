"""Session-continuation semantics for echoed `run_id`s (Slice 2).

`openspec/changes/agent-chat-history-ui`: the SPA echoes the SESSION id as
`run_id` so a turn appends to the conversation it belongs to. The agent's
decision tree, in priority order:

  1. Live run row (`agent.runs` non-terminal) → crash-recovery resume.
  2. Live session (`chat_sessions`, non-deleted) → continuation: same id
     echoed, fresh plan, NO run-state row minted/mutated. There is no
     idle-time chopping — an echoed id is a deliberate action (the user
     clicked the conversation or typed in it) and must keep its thread.
  3. Otherwise (unknown id, cross-user id, or soft-deleted session) →
     fresh session with a newly minted id.

These tests pin all three branches plus the recording rule (a
continuation records only the LAST user message — the SPA re-sends full
history every turn) and the disconnect-shield regression: terminal
persistence survives the SPA closing the stream at `terminator`.

Gated on `TEST_ASYNC_PG_DSN` (self-skips without a live Postgres).
"""
from __future__ import annotations

import json as _json
import os
import uuid as _uuid

import asyncpg
import fakeredis.aioredis as fakeaioredis
import httpx
import pytest

_DSN = os.environ.get("TEST_ASYNC_PG_DSN")

pytestmark = pytest.mark.skipif(
    not _DSN,
    reason="TEST_ASYNC_PG_DSN not set; needs a live Postgres",
)


async def _drop_schemas() -> None:
    conn = await asyncpg.connect(dsn=_DSN)
    try:
        await conn.execute("DROP SCHEMA IF EXISTS langgraph CASCADE")
        await conn.execute("DROP SCHEMA IF EXISTS agent CASCADE")
    finally:
        await conn.close()


class _StubStructuredChain:
    def __init__(self, plan_payload: dict):
        self._payload = plan_payload

    def __or__(self, other):
        return self

    async def ainvoke(self, messages, config=None):
        from agent.types import Plan

        return Plan.model_validate(self._payload)


class _StubLlm:
    """One llm step + a canned answer text (planner call 1, executor 2+)."""

    _PLAN = {"steps": [{"id": "step_1", "kind": "llm", "prompt": "answer"}]}
    call_count = 0

    def __init__(self, *args, **kwargs):
        pass

    async def ainvoke(self, messages):
        from langchain_core.messages import AIMessage

        type(self).call_count += 1
        if type(self).call_count == 1:
            return AIMessage(content=_json.dumps(self._PLAN))
        return AIMessage(
            content="stub answer",
            usage_metadata={
                "input_tokens": 7,
                "output_tokens": 3,
                "total_tokens": 10,
            },
        )

    def bind(self, **kwargs):
        type(self).call_count += 1
        return _StubStructuredChain(self._PLAN)


def _patch_stubs(monkeypatch):
    import agent.executor as executor_module
    import agent.planner as planner_module
    import agent.rag_client as rag_module
    import agent.tools_client as tools_module

    class _StubRag:
        async def retrieve(self, query):
            from agent.rag_client import RagResult

            return RagResult(context_block="stub context", citations=[])

    class _StubTools:
        async def call(self, name, arguments):
            return {"items": []}

        async def list_tools(self):
            return []

    monkeypatch.setattr(rag_module, "RagClient", lambda *a, **kw: _StubRag())
    monkeypatch.setattr(tools_module, "ToolsClient", lambda *a, **kw: _StubTools())
    monkeypatch.setattr(planner_module, "ChatOpenAI", _StubLlm)
    monkeypatch.setattr(executor_module, "ChatOpenAI", _StubLlm)

    from agent import observability as obs_module

    def _noop_setup(settings):
        from opentelemetry import trace
        from opentelemetry.sdk.trace import TracerProvider

        provider = TracerProvider()
        trace.set_tracer_provider(provider)
        return trace.get_tracer("nyx-agent-test")

    monkeypatch.setattr(obs_module, "setup_observability", _noop_setup)


@pytest.fixture
async def app_ctx(monkeypatch):
    """Live app: real lifespan, real DB, migrations applied, crons off."""
    await _drop_schemas()

    import agent.config as config_module

    settings = config_module.Settings(
        _env_file=None,
        database_url=_DSN,
        llm_base_url="http://localhost:9999/v1",
        metrics_port=0,
        retention_enabled=False,
        sweep_enabled=False,
        chat_retention_enabled=False,
    )
    monkeypatch.setattr(config_module, "_settings", settings)
    monkeypatch.setattr(config_module, "get_settings", lambda: settings)
    import agent.main as main_module

    monkeypatch.setattr(main_module, "get_settings", lambda: settings)

    _patch_stubs(monkeypatch)

    from agent.budget import BudgetGuard
    from agent.main import create_app, lifespan

    app = create_app()
    async with lifespan(app):
        redis = fakeaioredis.FakeRedis(decode_responses=True)
        app.state.budget = BudgetGuard(redis=redis, cap=1_000_000)
        try:
            yield app
        finally:
            await redis.aclose()


async def _post_chat(app, body, user_id: int = 42) -> httpx.Response:
    transport = httpx.ASGITransport(app=app)
    async with httpx.AsyncClient(
        transport=transport, base_url="http://test", timeout=30.0
    ) as client:
        return await client.post(
            "/agent/run",
            json=body,
            headers={"X-Nyx-User-Id": str(user_id), "Authorization": "Bearer stub"},
        )


async def _post_session(app, user_id: int = 42) -> str:
    transport = httpx.ASGITransport(app=app)
    async with httpx.AsyncClient(
        transport=transport, base_url="http://test", timeout=30.0
    ) as client:
        resp = await client.post(
            "/agent/sessions", headers={"X-Nyx-User-Id": str(user_id)}
        )
        assert resp.status_code == 200, resp.text
        return resp.json()["session_id"]


def _first_run_id(sse_text: str) -> str:
    for frame in sse_text.split("\n\n"):
        if frame.startswith("event: run_id"):
            data_line = [l for l in frame.splitlines() if l.startswith("data: ")][0]
            return _json.loads(data_line[len("data: ") :])["run_id"]
    raise AssertionError(f"no run_id frame in SSE text: {sse_text[:400]!r}")


async def _db():
    return await asyncpg.connect(
        dsn=_DSN, server_settings={"search_path": "agent,langgraph"}
    )


async def test_continuation_appends_to_same_session(app_ctx):
    """Turn 1 mints S; turn 2 echoes S with full history → same session, one
    new user row (the last message only), one new assistant row, no new
    agent.runs row."""
    _StubLlm.call_count = 0
    r1 = await _post_chat(app_ctx, {"messages": [{"role": "user", "content": "u1"}]})
    assert r1.status_code == 200
    session_id = _first_run_id(r1.text)

    _StubLlm.call_count = 0
    r2 = await _post_chat(
        app_ctx,
        {
            "run_id": session_id,
            "messages": [
                {"role": "user", "content": "u1"},
                {"role": "assistant", "content": "stub answer"},
                {"role": "user", "content": "u2"},
            ],
        },
    )
    assert r2.status_code == 200
    assert _first_run_id(r2.text) == session_id, (
        "the echoed session id must be preserved on a continuation"
    )

    conn = await _db()
    try:
        rows = await conn.fetch(
            "SELECT role, content FROM chat_messages WHERE session_id=$1 "
            "ORDER BY id ASC",
            _uuid.UUID(session_id),
        )
        contents = [(r["role"], r["content"]) for r in rows]
        assert contents == [
            ("user", "u1"),
            ("assistant", "stub answer"),
            ("user", "u2"),
            ("assistant", "stub answer"),
        ], f"no duplicates, exact appends; got {contents}"

        runs = await conn.fetchval(
            "SELECT count(*) FROM runs WHERE id=$1", _uuid.UUID(session_id)
        )
        assert runs == 1, "continuation must NOT mint a second runs row"
    finally:
        await conn.close()


async def test_api_created_session_receives_messages(app_ctx):
    """A session minted by POST /agent/sessions has no agent.runs row; its
    first turn must still land in it."""
    _StubLlm.call_count = 0
    session_id = await _post_session(app_ctx)

    r = await _post_chat(
        app_ctx,
        {"run_id": session_id, "messages": [{"role": "user", "content": "hello"}]},
    )
    assert r.status_code == 200
    assert _first_run_id(r.text) == session_id

    conn = await _db()
    try:
        count = await conn.fetchval(
            "SELECT count(*) FROM chat_messages WHERE session_id=$1",
            _uuid.UUID(session_id),
        )
        assert count == 2, "user + assistant rows in the API-created session"
        runs = await conn.fetchval(
            "SELECT count(*) FROM runs WHERE id=$1", _uuid.UUID(session_id)
        )
        assert runs == 0, "no phantom runs row for a session-only execution"
    finally:
        await conn.close()


async def test_old_session_still_continues(app_ctx):
    """No idle-time chopping: an echoed id keeps its thread regardless of
    age — the SPA starts a new session only via `+ New chat` (there is no
    idle boundary; review 2026-10-14 removed it so a returning user can
    always resume an old conversation)."""
    _StubLlm.call_count = 0
    session_id = await _post_session(app_ctx)

    conn = await _db()
    try:
        await conn.execute(
            "UPDATE chat_sessions SET last_active_at = now() - interval '30 days' "
            "WHERE id=$1",
            _uuid.UUID(session_id),
        )
    finally:
        await conn.close()

    _StubLlm.call_count = 0
    r = await _post_chat(
        app_ctx,
        {"run_id": session_id, "messages": [{"role": "user", "content": "later"}]},
    )
    assert r.status_code == 200
    assert _first_run_id(r.text) == session_id, (
        "an old session must continue — no idle-time session chopping"
    )

    conn = await _db()
    try:
        count = await conn.fetchval(
            "SELECT count(*) FROM chat_messages WHERE session_id=$1",
            _uuid.UUID(session_id),
        )
        assert count == 2, "the turn appended to the old session"
    finally:
        await conn.close()


async def test_unknown_and_cross_user_ids_stay_fresh(app_ctx):
    """Unknown ids and another user's session ids fall through to fresh."""
    unknown = str(_uuid.uuid4())
    _StubLlm.call_count = 0
    r = await _post_chat(
        app_ctx,
        {"run_id": unknown, "messages": [{"role": "user", "content": "x"}]},
    )
    assert _first_run_id(r.text) != unknown

    _StubLlm.call_count = 0
    foreign = await _post_session(app_ctx, user_id=42)
    _StubLlm.call_count = 0
    r2 = await _post_chat(
        app_ctx,
        {"run_id": foreign, "messages": [{"role": "user", "content": "y"}]},
        user_id=43,
    )
    assert _first_run_id(r2.text) != foreign, "cross-user echo must not continue"

    conn = await _db()
    try:
        count = await conn.fetchval(
            "SELECT count(*) FROM chat_messages WHERE session_id=$1",
            _uuid.UUID(foreign),
        )
        assert count == 0, "the foreign session must stay untouched"
    finally:
        await conn.close()


async def test_disconnect_during_finalize_still_persists(app_ctx):
    """Client disconnect while terminal persistence is mid-write must not
    lose the assistant row or the terminal run status (the finalize
    shield in `_stream_agent_run` keeps the detached task alive).

    Regression pin for the live 2026-10-14 failure: the SPA closes the
    stream on `terminator`; Go tears down the upstream request; the
    generator's finally-block writes were cancelled at their first
    await — chat_messages kept only the user row and agent.runs stayed
    'running' for a completed answer.
    """
    import asyncio as _asyncio

    _StubLlm.call_count = 0
    store = app_ctx.state.chat_store
    real_record = store.record_message
    entered = _asyncio.Event()

    async def slow_record(*args, **kwargs):
        if kwargs.get("role") == "assistant":
            entered.set()
            await _asyncio.sleep(0.5)
        return await real_record(*args, **kwargs)

    # Instance-attribute shadow: only this test's app sees the slow write.
    store.record_message = slow_record  # type: ignore[method-assign]

    transport = httpx.ASGITransport(app=app_ctx)
    async with httpx.AsyncClient(
        transport=transport, base_url="http://test", timeout=30.0
    ) as client:
        task = _asyncio.create_task(
            client.post(
                "/agent/run",
                json={"messages": [{"role": "user", "content": "hi"}]},
                headers={"X-Nyx-User-Id": "42", "Authorization": "Bearer stub"},
            )
        )
        await _asyncio.wait_for(entered.wait(), timeout=30)
        task.cancel()  # the client went away mid-finalize
        try:
            await task
        except BaseException:  # noqa: BLE001 — the cancel is the point
            pass

    # Give the detached finalize task time to finish its shielded write.
    await _asyncio.sleep(1.0)

    conn = await _db()
    try:
        rows = await conn.fetch(
            "SELECT role, steps::text AS steps FROM chat_messages ORDER BY id"
        )
        roles = [r["role"] for r in rows]
        assert "assistant" in roles, (
            f"assistant row must survive a client disconnect mid-finalize; got {roles}"
        )
        assistant = next(r for r in rows if r["role"] == "assistant")
        assert _json.loads(assistant["steps"]) == [
            {"id": "step_1", "kind": "llm", "status": "ok"}
        ]
        status = await conn.fetchval("SELECT status FROM runs LIMIT 1")
        assert status == "completed", (
            f"the terminal run transition must survive the disconnect; got {status}"
        )
    finally:
        await conn.close()


async def test_assistant_row_persists_usage_split(app_ctx):
    _StubLlm.call_count = 0
    resp = await _post_chat(
        app_ctx, {"messages": [{"role": "user", "content": "hi"}]}
    )
    assert resp.status_code == 200
    assert "event: done" in resp.text

    conn = await _db()
    try:
        row = await conn.fetchrow(
            "SELECT tokens, prompt_tokens, completion_tokens "
            "FROM chat_messages WHERE role='assistant' "
            "ORDER BY id DESC LIMIT 1"
        )
    finally:
        await conn.close()

    assert row is not None
    assert row["prompt_tokens"] == 7
    assert row["completion_tokens"] == 3
    assert row["tokens"] == 10


async def test_planned_run_persists_step_trace(app_ctx):
    """A completed run folds the frames it forwarded into the assistant
    row's `steps`; the user row stays NULL (agent-chat-step-replay 3.2)."""
    _StubLlm.call_count = 0
    resp = await _post_chat(
        app_ctx, {"messages": [{"role": "user", "content": "hi"}]}
    )
    assert resp.status_code == 200
    assert "event: plan" in resp.text
    assert "event: step_end" in resp.text

    conn = await _db()
    try:
        rows = await conn.fetch(
            "SELECT role, steps::text AS steps FROM chat_messages "
            "ORDER BY id ASC"
        )
    finally:
        await conn.close()

    assistant = next(row for row in rows if row["role"] == "assistant")
    user = next(row for row in rows if row["role"] == "user")
    assert _json.loads(assistant["steps"]) == [
        {"id": "step_1", "kind": "llm", "status": "ok"}
    ]
    assert user["steps"] is None


async def test_no_plan_run_persists_null_steps(app_ctx, monkeypatch):
    """A completed run that forwarded no plan frame leaves `steps` NULL
    while still persisting its assistant text."""
    from agent import main as main_module
    from agent.types import Event

    async def _stub_pump(state, *, sink, **kwargs):
        try:
            await sink.flush(
                [
                    Event(kind="delta", payload={"delta": "plain answer"}),
                    Event(
                        kind="done",
                        payload={
                            "usage": {
                                "prompt_tokens": 1,
                                "completion_tokens": 1,
                                "total_tokens": 2,
                            }
                        },
                    ),
                ]
            )
        finally:
            await sink.close()

    monkeypatch.setattr(main_module, "_pump_events", _stub_pump)
    _StubLlm.call_count = 0
    resp = await _post_chat(
        app_ctx, {"messages": [{"role": "user", "content": "hi"}]}
    )
    assert resp.status_code == 200
    assert "event: plan" not in resp.text

    conn = await _db()
    try:
        row = await conn.fetchrow(
            "SELECT content, steps::text AS steps FROM chat_messages "
            "WHERE role='assistant' ORDER BY id DESC LIMIT 1"
        )
    finally:
        await conn.close()

    assert row is not None
    assert row["content"] == "plain answer"
    assert row["steps"] is None


async def test_step_trace_serialization_failure_degrades_to_null(
    app_ctx, monkeypatch
):
    """A non-JSON-safe trace is dropped with a WARN; the assistant row and
    its text still persist (design.md D6)."""
    from agent.step_trace import StepTrace

    monkeypatch.setattr(
        StepTrace,
        "to_list",
        lambda self: [
            {
                "id": "step_1",
                "kind": "llm",
                "status": "ok",
                "tool_result": {1, 2},
            }
        ],
    )

    _StubLlm.call_count = 0
    resp = await _post_chat(
        app_ctx, {"messages": [{"role": "user", "content": "hi"}]}
    )
    assert resp.status_code == 200
    assert "event: done" in resp.text

    conn = await _db()
    try:
        row = await conn.fetchrow(
            "SELECT content, steps::text AS steps FROM chat_messages "
            "WHERE role='assistant' ORDER BY id DESC LIMIT 1"
        )
    finally:
        await conn.close()

    assert row is not None
    assert row["content"] == "stub answer"
    assert row["steps"] is None


async def test_step_trace_db_rejection_degrades_to_null_and_ends_session(
    app_ctx, monkeypatch
):
    """Postgres rejects a NUL escape in the steps jsonb (SQLSTATE 22P05);
    the store retries without the trace and the session still ends."""
    from agent.step_trace import StepTrace

    monkeypatch.setattr(
        StepTrace,
        "to_list",
        lambda self: [
            {
                "id": "step_1",
                "kind": "llm",
                "status": "ok",
                "tool_result": "boom\x00",
            }
        ],
    )

    _StubLlm.call_count = 0
    resp = await _post_chat(
        app_ctx, {"messages": [{"role": "user", "content": "hi"}]}
    )
    assert resp.status_code == 200
    assert "event: done" in resp.text

    conn = await _db()
    try:
        row = await conn.fetchrow(
            "SELECT m.content, m.steps::text AS steps, s.ended_at "
            "FROM chat_messages m JOIN chat_sessions s ON s.id = m.session_id "
            "WHERE m.role='assistant' ORDER BY m.id DESC LIMIT 1"
        )
    finally:
        await conn.close()

    assert row is not None
    assert row["content"] == "stub answer"
    assert row["steps"] is None
    assert row["ended_at"] is not None
