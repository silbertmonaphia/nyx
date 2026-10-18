"""Session titles: deterministic first-exchange label + one-shot refinement.

`openspec/changes/agent-chat-fixes` tasks 2.2/2.3. The pipeline has two
stages, both pinned here:

  1. DETERMINISTIC (2.2): the session-init block derives a label from the
     session's first user message — whitespace collapsed (newlines
     included), trimmed, truncated to `_TITLE_MAX_CHARS` CODE POINTS
     (CJK-safe: Python slicing never splits a multi-byte character), and
     writes it through `set_title_if_empty`. The `title=''` predicate is
     what makes the run that wrote it "the session's first exchange" —
     a continuation on a titled session, a resumed run, and the second
     turn of a conversation all lose that race by construction. A store
     fault is warn-logged and the run proceeds untouched.
  2. REFINEMENT (2.3): the winner of that race spawns ONE detached,
     bounded `ChatOpenAI` call after the reply is finalized, which
     overwrites the label through the ownership + `deleted_at IS NULL`
     guarded `set_title`. Any failure (client, LLM, unusable output,
     store) keeps the deterministic label, logs a WARN, and never
     retries. The refinement consults no budget — it is internal work,
     not user-visible chat traffic.

Pattern mirrors `test_session_continuation.py` (live DB, migrations via
the real lifespan, stubbed planner/executor/rag/tools clients) plus a
stub for `agent.main.ChatOpenAI`, the refinement's call site. The
pure-function cases (`_derive_session_title`, `_should_refine_title`)
live in `tests/test_run.py`, which runs without a database.

Gated on `TEST_ASYNC_PG_DSN` (self-skips without a live Postgres).
"""
from __future__ import annotations

import asyncio
import json as _json
import logging
import os
from typing import ClassVar

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

    _PLAN: ClassVar[dict] = {
        "steps": [{"id": "step_1", "kind": "llm", "prompt": "answer"}]
    }
    call_count = 0

    def __init__(self, *args, **kwargs):
        pass

    async def ainvoke(self, messages, config=None):
        from langchain_core.messages import AIMessage

        type(self).call_count += 1
        if type(self).call_count == 1:
            return AIMessage(content=_json.dumps(self._PLAN))
        return AIMessage(content="stub answer")

    def bind(self, **kwargs):
        type(self).call_count += 1
        return _StubStructuredChain(self._PLAN)


class _StubTitleLlm:
    """`agent.main.ChatOpenAI` stub: the refinement call site.

    Records the constructor kwargs (the bounded max_tokens/timeout the
    design pins) and the prompt messages, then answers with `title`.
    """

    title: ClassVar[str] = "生鲜电商推荐"
    call_count = 0
    init_kwargs: ClassVar[list] = []
    prompts: ClassVar[list] = []

    def __init__(self, *args, **kwargs):
        type(self).init_kwargs.append(kwargs)

    @classmethod
    def reset(cls) -> None:
        cls.call_count = 0
        cls.init_kwargs = []
        cls.prompts = []

    async def ainvoke(self, messages, config=None):
        from langchain_core.messages import AIMessage

        type(self).call_count += 1
        type(self).prompts.append(list(messages))
        return AIMessage(content=type(self).title)


class _BoomTitleLlm:
    call_count = 0

    def __init__(self, *args, **kwargs):
        pass

    @classmethod
    def reset(cls) -> None:
        cls.call_count = 0

    async def ainvoke(self, messages, config=None):
        type(self).call_count += 1
        raise RuntimeError("simulated refinement outage")


def _patch_stubs(monkeypatch):
    import agent.executor as executor_module
    import agent.main as main_module
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

    stub_rag = lambda *a, **kw: _StubRag()
    stub_tools = lambda *a, **kw: _StubTools()

    monkeypatch.setattr(rag_module, "RagClient", stub_rag)
    monkeypatch.setattr(tools_module, "ToolsClient", stub_tools)
    # `main.py` binds both classes at import, so the module-attribute
    # patch above does not reach its call sites — without these the run
    # path would dial the real (absent) rag-bridge / MCP services.
    monkeypatch.setattr(main_module, "RagClient", stub_rag)
    monkeypatch.setattr(main_module, "ToolsClient", stub_tools)
    monkeypatch.setattr(planner_module, "ChatOpenAI", _StubLlm)
    monkeypatch.setattr(executor_module, "ChatOpenAI", _StubLlm)
    monkeypatch.setattr(main_module, "ChatOpenAI", _StubTitleLlm)

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
            data_line = next(
                l for l in frame.splitlines() if l.startswith("data: ")
            )
            return _json.loads(data_line[len("data: ") :])["run_id"]
    raise AssertionError(f"no run_id frame in SSE text: {sse_text[:400]!r}")


async def _title(app, session_id, user_id: int = 42) -> str:
    session = await app.state.chat_store.get_session(session_id, user_id)
    assert session is not None, f"session {session_id} vanished"
    return session.title


async def _drain_title_tasks(timeout: float = 30.0) -> None:
    """Await the module-level tracked refinement tasks.

    Same hygiene as `_PREFS_TASKS` in `test_prefs_injection.py`: a
    dangling `create_task` would otherwise produce "task was destroyed but
    it is pending" noise, and the assertions here want the refinement's
    write to have landed (or provably failed) before they run.
    """
    from agent.main import _TITLE_TASKS

    loop = asyncio.get_running_loop()
    deadline = loop.time() + timeout
    while _TITLE_TASKS:
        if loop.time() > deadline:
            raise AssertionError(
                f"title-refinement tasks did not drain: {len(_TITLE_TASKS)} pending"
            )
        await asyncio.sleep(0.05)


def _warnings(caplog) -> list[str]:
    return [
        rec.getMessage()
        for rec in caplog.records
        if rec.levelno >= logging.WARNING
    ]


# --- deterministic derivation (2.2) ---------------------------------------
#
# The pure-function cases (`_derive_session_title`, `_should_refine_title`)
# live in `tests/test_run.py`, which runs without a database; these cover
# the wiring that needs a live app.


# --- deterministic title on the wire (2.2) --------------------------------


async def test_first_message_titles_the_session(app_ctx, monkeypatch) -> None:
    """A fresh run's first user message becomes the session title, capped
    at 48 characters; the recorded message keeps the full text.

    The refinement is deliberately broken here (`_BoomTitleLlm`) so the
    assertion reads the DETERMINISTIC label, not the refined one."""
    import agent.main as main_module
    from agent.main import _TITLE_MAX_CHARS

    monkeypatch.setattr(main_module, "ChatOpenAI", _BoomTitleLlm)
    _BoomTitleLlm.reset()

    long_cjk = "熬夜加班" * 50
    _StubLlm.call_count = 0
    r = await _post_chat(app_ctx, {"messages": [{"role": "user", "content": long_cjk}]})
    assert r.status_code == 200
    assert "event: terminator" in r.text
    assert "event: error" not in r.text

    run_id = _first_run_id(r.text)
    await _drain_title_tasks()

    assert await _title(app_ctx, run_id) == long_cjk[:_TITLE_MAX_CHARS]

    messages = await app_ctx.state.chat_store.list_messages(run_id, 42)
    assert messages is not None
    user_rows = [m for m in messages if m.role == "user"]
    assert [m.content for m in user_rows] == [long_cjk], (
        "the session title is a label; the stored message is not truncated"
    )


async def test_title_store_failure_degrades_silently(
    app_ctx, monkeypatch, caplog
) -> None:
    """Spec "Chat store unavailable degrades silently": the write raises,
    the run still completes with its answer, a WARN is logged, and no
    refinement is spawned (nothing was titled)."""
    from agent.chat_store import ChatStoreUnavailable
    from agent.main import _TITLE_TASKS

    async def _boom(session_id, user_id, title):
        raise ChatStoreUnavailable("simulated store outage")

    monkeypatch.setattr(app_ctx.state.chat_store, "set_title_if_empty", _boom)

    _StubLlm.call_count = 0
    with caplog.at_level(logging.WARNING):
        r = await _post_chat(
            app_ctx, {"messages": [{"role": "user", "content": "整理我的订阅"}]}
        )

    assert r.status_code == 200
    assert "event: terminator" in r.text
    assert "event: error" not in r.text
    assert "stub answer" in r.text, "the reply completed normally"

    await _drain_title_tasks()
    assert not _TITLE_TASKS, "a failed write must spawn no refinement"
    assert any("set_title_if_empty failed" in w for w in _warnings(caplog)), (
        f"the store failure must be warn-logged; saw {_warnings(caplog)}"
    )


# --- refinement (2.3) -----------------------------------------------------


async def test_first_exchange_refines_the_title(app_ctx) -> None:
    """The winning run refines the deterministic label with ONE small
    bounded call after the reply and overwrites it via `set_title`."""
    from agent.main import (
        _TITLE_MAX_CHARS,
        _TITLE_REFINEMENT_MAX_TOKENS,
        _TITLE_REFINEMENT_TIMEOUT_SECONDS,
    )

    _StubLlm.call_count = 0
    _StubTitleLlm.reset()

    first_msg = "帮我看看最近三个月的生鲜订阅" + "还有哪些" * 20
    r = await _post_chat(app_ctx, {"messages": [{"role": "user", "content": first_msg}]})
    assert r.status_code == 200
    assert "event: terminator" in r.text
    run_id = _first_run_id(r.text)

    await _drain_title_tasks()

    assert await _title(app_ctx, run_id) == "生鲜电商推荐"
    assert _StubTitleLlm.call_count == 1, "exactly one refinement call (no retry)"

    kwargs = _StubTitleLlm.init_kwargs[0]
    assert kwargs["model"] == app_ctx.state.settings.planner_model
    assert kwargs["max_tokens"] == _TITLE_REFINEMENT_MAX_TOKENS, (
        "the refinement must be token-bounded"
    )
    assert kwargs["timeout"] == _TITLE_REFINEMENT_TIMEOUT_SECONDS, (
        "the refinement must be time-bounded"
    )
    assert kwargs["temperature"] == 0

    system, human = _StubTitleLlm.prompts[0]
    assert system.type == "system" and "title" in system.content.lower()
    assert first_msg[:_TITLE_MAX_CHARS] in human.content, (
        "the prompt carries the first user message"
    )
    assert "stub answer" in human.content, "the prompt carries the assistant reply"


async def test_precreated_session_first_turn_still_refines(app_ctx) -> None:
    """The SPA's `+ New chat` pre-creates the session (`POST /agent/
    sessions`), so its first turn arrives as a session CONTINUATION —
    yet it is still the session's first exchange, so it titles and
    refines. The `title=''` guard, not the run shape, is what decides."""
    session_id = await _post_session(app_ctx)

    _StubLlm.call_count = 0
    _StubTitleLlm.reset()
    r = await _post_chat(
        app_ctx,
        {
            "run_id": session_id,
            "messages": [{"role": "user", "content": "帮我看看最近的订阅"}],
        },
    )
    assert r.status_code == 200
    assert "event: terminator" in r.text
    assert _first_run_id(r.text) == session_id, "the echoed session id is kept"

    await _drain_title_tasks()

    assert await _title(app_ctx, session_id) == "生鲜电商推荐"
    assert _StubTitleLlm.call_count == 1


async def test_continuation_never_touches_the_title(app_ctx) -> None:
    """A second turn on a titled session neither rewrites the title nor
    spawns a refinement — the deterministic write loses the `title=''`
    race, so "at most once per session" holds structurally."""
    from agent.main import _TITLE_TASKS

    session_id = await _post_session(app_ctx)
    await app_ctx.state.chat_store.set_title_if_empty(
        session_id, 42, "旧标题"
    )

    _StubLlm.call_count = 0
    _StubTitleLlm.reset()
    r = await _post_chat(
        app_ctx,
        {
            "run_id": session_id,
            "messages": [
                {"role": "user", "content": "帮我看看最近的订阅"},
                {"role": "assistant", "content": "好的"},
                {"role": "user", "content": "再总结一下"},
            ],
        },
    )
    assert r.status_code == 200
    assert "event: terminator" in r.text

    await _drain_title_tasks()
    assert not _TITLE_TASKS, "a continuation must spawn no refinement"
    assert _StubTitleLlm.call_count == 0
    assert await _title(app_ctx, session_id) == "旧标题"


async def test_resumed_run_never_titles_or_refines(app_ctx) -> None:
    """A crash-recovery resume skips the session-init writes entirely (the
    message was recorded at the original request) — so it must not title
    the session nor spawn a refinement."""
    from agent.main import _TITLE_TASKS

    run_id = await app_ctx.state.run_metadata.create_run(42)

    _StubLlm.call_count = 0
    _StubTitleLlm.reset()
    r = await _post_chat(
        app_ctx,
        {
            "run_id": run_id,
            "messages": [{"role": "user", "content": "帮我看看最近的订阅"}],
        },
    )
    assert r.status_code == 200
    assert '"resumed"' in r.text, r.text[:400]

    await _drain_title_tasks()
    assert not _TITLE_TASKS
    assert _StubTitleLlm.call_count == 0
    assert await _title(app_ctx, run_id) == ""


async def test_refinement_failure_keeps_the_deterministic_title(
    app_ctx, monkeypatch, caplog
) -> None:
    """Spec "Refinement failure is invisible": a raising refinement LLM
    leaves the deterministic label in place, logs a WARN, and does not
    retry — the stream was already complete and is unaffected."""
    import agent.main as main_module

    monkeypatch.setattr(main_module, "ChatOpenAI", _BoomTitleLlm)
    _BoomTitleLlm.reset()

    _StubLlm.call_count = 0
    with caplog.at_level(logging.WARNING):
        r = await _post_chat(
            app_ctx,
            {"messages": [{"role": "user", "content": "整理我的订阅列表"}]},
        )
        assert r.status_code == 200
        assert "event: terminator" in r.text
        run_id = _first_run_id(r.text)
        await _drain_title_tasks()

    assert _BoomTitleLlm.call_count == 1, "no retry after a refinement failure"
    assert await _title(app_ctx, run_id) == "整理我的订阅列表"
    assert any("title refinement failed" in w for w in _warnings(caplog)), (
        f"the failure must be warn-logged; saw {_warnings(caplog)}"
    )


async def test_slow_refinement_never_delays_or_alters_the_stream(
    app_ctx, monkeypatch
) -> None:
    """Spec "Slow refinement never delays the stream": the reply's frames
    are complete — and no frame carries refinement state — long before the
    refinement finishes; the title lands afterwards."""
    import agent.main as main_module

    gate = asyncio.Event()

    class _SlowTitleLlm:
        started = 0

        def __init__(self, *args, **kwargs):
            pass

        async def ainvoke(self, messages, config=None):
            from langchain_core.messages import AIMessage

            type(self).started += 1
            await gate.wait()
            return AIMessage(content="生鲜电商推荐")

    monkeypatch.setattr(main_module, "ChatOpenAI", _SlowTitleLlm)

    _StubLlm.call_count = 0
    loop = asyncio.get_running_loop()
    started_at = loop.time()
    r = await _post_chat(
        app_ctx, {"messages": [{"role": "user", "content": "整理我的订阅列表"}]}
    )
    elapsed = loop.time() - started_at

    assert r.status_code == 200
    assert "event: terminator" in r.text
    assert "data: [DONE]" in r.text
    assert "生鲜电商推荐" not in r.text, "no frame carries refinement state"
    assert elapsed < 5.0, f"the reply must not wait on the refinement ({elapsed:.1f}s)"

    gate.set()
    await _drain_title_tasks()

    assert _SlowTitleLlm.started == 1
    run_id = _first_run_id(r.text)
    assert await _title(app_ctx, run_id) == "生鲜电商推荐", (
        "the title lands after the stream, on the next list refresh"
    )


async def test_refinement_never_consults_the_budget(app_ctx) -> None:
    """Spec "Refinement never consumes the user chat budget": the budget
    guard must see exactly the run's own chat-call consultations, with
    zero extra for the refinement model call.

    Pinned DIFFERENTIALLY (two runs whose chat-call shapes are identical,
    one of which refines) so the assertion holds regardless of how many
    times a run internally consults the guard.
    """
    real_budget = app_ctx.state.budget
    seen: list[int] = []

    class _SpyBudget:
        def __getattr__(self, name):
            return getattr(real_budget, name)

        async def check_and_increment(self, user_id, tokens, tracer):
            seen.append(user_id)
            return await real_budget.check_and_increment(user_id, tokens, tracer)

    app_ctx.state.budget = _SpyBudget()

    # Run A: a continuation of an already-titled session — no title write,
    # no refinement.
    titled = await _post_session(app_ctx)
    await app_ctx.state.chat_store.set_title_if_empty(titled, 42, "旧标题")
    _StubLlm.call_count = 0
    ra = await _post_chat(
        app_ctx,
        {"run_id": titled, "messages": [{"role": "user", "content": "再总结一下"}]},
    )
    assert ra.status_code == 200 and "event: terminator" in ra.text
    await _drain_title_tasks()
    consultations_without_refinement = len(seen)

    # Run B: a fresh session — same plan shape, refinement fires.
    seen.clear()
    _StubLlm.call_count = 0
    _StubTitleLlm.reset()
    rb = await _post_chat(
        app_ctx, {"messages": [{"role": "user", "content": "整理我的订阅列表"}]}
    )
    assert rb.status_code == 200 and "event: terminator" in rb.text
    await _drain_title_tasks()

    assert _StubTitleLlm.call_count == 1, "the refinement model call happened"
    assert len(seen) == consultations_without_refinement, (
        f"budget consulted {len(seen)} vs {consultations_without_refinement} "
        "without the refinement — the refinement must not check the budget guard"
    )
