"""Wiring tests for the user-preference pipeline.

`openspec/changes/agent-user-preferences` (Slice 3). This file pins the
WIRING half of the change — the core module's own coverage lives in
`tests/test_prefs.py`:

  1. a preference-bearing turn reaches `done`/`terminator` first, then a
     tracked extraction task upserts prefs rows (design D2: async, after
     the reply, never in its path);
  2. an extraction failure is invisible: the run still reaches the
     terminator, a WARN is logged, no rows are written;
  3. `PYTHON_AGENT_PREFS_ENABLED=false` spawns no task and writes no
     rows (the kill switch disables extraction AND injection together);
  4. planner prompt parity: `prefs_text=""` composes the pre-injection
     system prompt byte-for-byte; a non-empty block appends after the
     static base + web-tools block; the retry re-prompt carries it;
  5. the gate skips small talk: "今天天气怎么样" makes no extraction call;
  6. the executor (openspec/changes/agent-chat-fixes 1.1/1.2) carries the
     block on the answer path, and omits it byte-identically when there
     are no prefs or the kill switch is on (the unit-level prompt-parity
     matrix lives in `tests/test_executor.py`).

Pattern mirrors `test_session_continuation.py` (live DB, migrations via
the real lifespan, stubbed planner/executor/rag/tools clients). The
extraction call is stubbed at `agent.prefs.ChatOpenAI`, so the core's
`extract_preferences` — gate, prompt, parse, validate — runs for real
against a canned payload.

Gated on `TEST_ASYNC_PG_DSN` (self-skips without a live Postgres).
"""
from __future__ import annotations

import asyncio
import json as _json
import logging
import os
from contextlib import asynccontextmanager
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


class _StubPrefsLlm:
    """Stub for `agent.prefs.ChatOpenAI` — a canned extraction payload.

    The core's `extract_preferences` runs for real: it constructs this
    stub, invokes it, and parses/validates the returned content.
    """

    PAYLOAD: ClassVar[list] = [
        {"key": "snack", "value": "鱿鱼丝", "confidence": 0.9}
    ]
    call_count = 0

    def __init__(self, *args, **kwargs):
        pass

    @classmethod
    def reset(cls) -> None:
        cls.call_count = 0

    async def ainvoke(self, messages, config=None):
        from langchain_core.messages import AIMessage

        type(self).call_count += 1
        return AIMessage(content=_json.dumps(self.PAYLOAD))


def _patch_stubs(monkeypatch):
    import agent.executor as executor_module
    import agent.planner as planner_module
    import agent.prefs as prefs_module
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
    monkeypatch.setattr(prefs_module, "ChatOpenAI", _StubPrefsLlm)

    from agent import observability as obs_module

    def _noop_setup(settings):
        from opentelemetry import trace
        from opentelemetry.sdk.trace import TracerProvider

        provider = TracerProvider()
        trace.set_tracer_provider(provider)
        return trace.get_tracer("nyx-agent-test")

    monkeypatch.setattr(obs_module, "setup_observability", _noop_setup)


@asynccontextmanager
async def _running_app(monkeypatch, **settings_overrides):
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
        **settings_overrides,
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


@pytest.fixture
async def app_ctx(monkeypatch):
    async with _running_app(monkeypatch) as app:
        yield app


@pytest.fixture
async def app_ctx_prefs_disabled(monkeypatch):
    """Same live app with the kill switch on (`PYTHON_AGENT_PREFS_ENABLED=false`).

    The PrefsStore itself stays wired (`app.state.prefs_store`): the kill
    switch must disable extraction/injection even when the store exists.
    """
    async with _running_app(monkeypatch, prefs_enabled=False) as app:
        yield app


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


def _first_run_id(sse_text: str) -> str:
    for frame in sse_text.split("\n\n"):
        if frame.startswith("event: run_id"):
            data_line = next(
                line for line in frame.splitlines() if line.startswith("data: ")
            )
            return _json.loads(data_line[len("data: ") :])["run_id"]
    raise AssertionError(f"no run_id frame in SSE text: {sse_text[:400]!r}")


async def _drain_prefs_tasks(timeout: float = 30.0) -> None:
    """Await the module-level tracked extraction tasks (design D2's
    test-hygiene note: a dangling `create_task` would otherwise produce
    "task was destroyed but it is pending" noise and flake)."""
    from agent.main import _PREFS_TASKS

    loop = asyncio.get_running_loop()
    deadline = loop.time() + timeout
    while _PREFS_TASKS:
        if loop.time() > deadline:
            raise AssertionError(
                "preference-extraction tasks did not drain: "
                f"{len(_PREFS_TASKS)} still pending"
            )
        await asyncio.sleep(0.05)


async def _stored_prefs(app, user_id: int = 42):
    return await app.state.prefs_store.load_prefs(user_id, limit=50)


def _settings(**overrides):
    """Settings for the pure-unit prompt-parity test (no DB/lifespan)."""
    from agent.config import Settings

    base = {
        "database_url": _DSN,
        "llm_base_url": "http://localhost:9999/v1",
        "llm_api_key": "test-key",
    }
    base.update(overrides)
    return Settings(_env_file=None, **base)


async def test_preference_turn_extracts_and_stores_after_reply(app_ctx) -> None:
    """The gate passes → the reply completes on the wire FIRST, then a
    tracked task extracts and upserts (design D2)."""
    _StubLlm.call_count = 0
    _StubPrefsLlm.reset()

    r = await _post_chat(
        app_ctx, {"messages": [{"role": "user", "content": "我喜欢鱿鱼丝"}]}
    )
    assert r.status_code == 200
    # The run reached a clean terminal state; no frame carried extraction
    # state (the extraction runs strictly after the reply).
    assert "event: terminator" in r.text
    assert "event: error" not in r.text
    run_id = _first_run_id(r.text)

    await _drain_prefs_tasks()

    prefs = await _stored_prefs(app_ctx)
    assert [(p.key, p.value) for p in prefs] == [("snack", "鱿鱼丝")]
    assert str(prefs[0].source_session_id) == run_id, (
        "the extraction is stamped with the run/session it came from"
    )
    assert _StubPrefsLlm.call_count == 1, (
        "exactly one small extraction call for one turn (D4)"
    )


async def test_extraction_failure_is_invisible(
    app_ctx, monkeypatch, caplog
) -> None:
    """A raising extraction client must not surface anywhere: the run
    reaches `terminator`, a WARN is logged, no rows are written."""
    import agent.prefs as prefs_module

    class _BoomPrefsLlm:
        def __init__(self, *args, **kwargs):
            pass

        async def ainvoke(self, messages, config=None):
            raise RuntimeError("simulated extraction outage")

    monkeypatch.setattr(prefs_module, "ChatOpenAI", _BoomPrefsLlm)

    _StubLlm.call_count = 0
    with caplog.at_level(logging.WARNING):
        r = await _post_chat(
            app_ctx, {"messages": [{"role": "user", "content": "我喜欢鱿鱼丝"}]}
        )

    assert r.status_code == 200
    assert "event: terminator" in r.text
    assert "event: error" not in r.text

    await _drain_prefs_tasks()

    assert await _stored_prefs(app_ctx) == []
    warnings = [
        rec.getMessage()
        for rec in caplog.records
        if rec.levelno >= logging.WARNING
        and "pref" in (rec.getMessage() or "").lower()
    ]
    assert warnings, (
        "the extraction failure must be warn-logged; records: "
        f"{[(r.levelname, r.getMessage()) for r in caplog.records]}"
    )


async def test_kill_switch_disables_extraction(app_ctx_prefs_disabled) -> None:
    """`PYTHON_AGENT_PREFS_ENABLED=false`: no task is spawned, no call is
    made, no rows are written — even though the store is wired."""
    from agent.main import _PREFS_TASKS

    _StubLlm.call_count = 0
    _StubPrefsLlm.reset()

    r = await _post_chat(
        app_ctx_prefs_disabled,
        {"messages": [{"role": "user", "content": "我喜欢鱿鱼丝"}]},
    )
    assert r.status_code == 200

    # The spawn is synchronous inside the generator's finally — by the
    # time the response is fully consumed, either it happened or it never
    # will, so no sleep/race is involved here.
    assert not _PREFS_TASKS, "the kill switch must spawn no extraction task"
    assert _StubPrefsLlm.call_count == 0, "the kill switch must make no LLM call"
    assert await _stored_prefs(app_ctx_prefs_disabled) == []


async def test_gate_miss_makes_no_extraction_call(app_ctx) -> None:
    """Small talk ("今天天气怎么样") fails the gate → no extraction LLM
    call, no task, no rows."""
    from agent.main import _PREFS_TASKS

    _StubLlm.call_count = 0
    _StubPrefsLlm.reset()

    r = await _post_chat(
        app_ctx, {"messages": [{"role": "user", "content": "今天天气怎么样"}]}
    )
    assert r.status_code == 200

    assert not _PREFS_TASKS
    assert _StubPrefsLlm.call_count == 0, (
        "the gate must skip the extraction LLM call entirely"
    )
    assert await _stored_prefs(app_ctx) == []


def test_prompt_parity_and_injection() -> None:
    """The planner system prompt is byte-identical when there are no
    prefs, and the rendered block appends when there are (D5)."""
    from agent import planner as planner_module
    from agent.prefs import Pref, prefs_block
    from agent.web_tools import planner_block

    settings = _settings()
    state = {"messages": [{"role": "user", "content": "hi"}]}
    # The pre-injection construction this slice must not disturb.
    baseline = planner_module.PLANNER_SYSTEM_PROMPT + planner_block(settings)

    assert planner_module._initial_messages(state, settings)[0].content == baseline
    assert (
        planner_module._initial_messages(state, settings, prefs_text="")[0].content
        == baseline
    )
    assert (
        planner_module._initial_messages(state, settings, prefs_text=None)[0].content
        == baseline
    )

    # Non-empty prefs append AFTER the static base + web-tools block.
    block = prefs_block([Pref(key="snack", value="鱿鱼丝")])
    assert block != ""
    keys = planner_module._initial_messages(state, settings, prefs_text=block)
    assert keys[0].content == baseline + block

    # The retry re-prompt mutates only the last message → the prefs block
    # rides along unchanged.
    retried = planner_module._append_error_to_messages(keys, "boom")
    assert retried[0].content == baseline + block


async def test_extraction_does_not_consult_the_budget_guard(app_ctx) -> None:
    """Spec "Extraction never consumes the user chat budget": the
    extraction is an internal operation — the budget guard must see
    exactly the run's own chat-call consultations, with zero extra for
    the extraction model call (design D4).

    The property is pinned DIFFERENTIALLY (two identical run shapes, one
    with extraction firing, one without) so the assertion holds
    regardless of how many times a run internally consults the guard.
    """
    _StubLlm.call_count = 0
    _StubPrefsLlm.reset()

    real_budget = app_ctx.state.budget
    seen: list[int] = []

    class _SpyBudget:
        def __getattr__(self, name):
            return getattr(real_budget, name)

        async def check_and_increment(self, user_id, tokens, tracer):
            seen.append(user_id)
            return await real_budget.check_and_increment(user_id, tokens, tracer)

    app_ctx.state.budget = _SpyBudget()

    # Run A: gate miss — no extraction task at all.
    _StubLlm.call_count = 0
    ra = await _post_chat(
        app_ctx, {"messages": [{"role": "user", "content": "今天天气怎么样"}]}
    )
    assert ra.status_code == 200 and "event: terminator" in ra.text
    await _drain_prefs_tasks()
    consultations_without_extraction = len(seen)

    # Run B: same stub-plan shape (call_count reset), extraction fires.
    seen.clear()  # count run B's consultations in isolation
    _StubLlm.call_count = 0
    rb = await _post_chat(
        app_ctx, {"messages": [{"role": "user", "content": "我喜欢鱿鱼丝"}]}
    )
    assert rb.status_code == 200 and "event: terminator" in rb.text
    await _drain_prefs_tasks()

    prefs = await _stored_prefs(app_ctx)
    assert [p.key for p in prefs] == ["snack"], "extraction still ran"

    extraction_calls = _StubPrefsLlm.call_count
    assert extraction_calls == 1, "the extraction model call happened"
    assert len(seen) == consultations_without_extraction, (
        f"budget consulted {len(seen)} vs {consultations_without_extraction} "
        "without extraction — extraction must not check the budget guard"
    )


async def test_injection_load_failure_degrades_quietly(
    app_ctx, monkeypatch
) -> None:
    """The prefs load on the injection path is best-effort (design D5):
    a store failure must degrade to "inject nothing" — the stream still
    completes with an answer and no error frame."""
    import agent.prefs as prefs_module

    store = app_ctx.state.prefs_store

    async def _boom(user_id, *, limit):
        raise prefs_module.PrefsStoreUnavailable("simulated store outage")

    monkeypatch.setattr(store, "load_prefs", _boom)

    _StubLlm.call_count = 0
    r = await _post_chat(
        app_ctx, {"messages": [{"role": "user", "content": "今天天气怎么样"}]}
    )
    assert r.status_code == 200
    assert "event: terminator" in r.text
    assert "event: error" not in r.text
    assert "stub answer" in r.text, "the run completed normally"


# --- executor injection (openspec/changes/agent-chat-fixes tasks 1.1/1.2) ----
#
# The executor writes the user-visible answer, so the rendered prefs
# block must reach BOTH answer paths — and the empty/kill-switch cases
# must leave those prompts byte-identical. The planner's own injection
# (above) is unchanged; these pin the second node end-to-end. The
# unit-level prompt matrix (both paths, empty/kill-switch parity, the
# factory closure) lives in `tests/test_executor.py`.


class _CapturingExecutorLlm:
    """Executor-side `ChatOpenAI` stub that records the message lists.

    Patched over `agent.executor.ChatOpenAI` only, so the planner still
    runs its own stub through `bind`/`ainvoke` and every capture here is
    a user-visible-answer call.
    """

    captured: ClassVar[list] = []

    def __init__(self, *args, **kwargs):
        pass

    def bind_tools(self, schemas):
        return self

    @classmethod
    def reset(cls) -> None:
        cls.captured = []

    async def ainvoke(self, messages, config=None):
        from langchain_core.messages import AIMessage

        type(self).captured.append(list(messages))
        return AIMessage(content="stub answer")


async def test_recall_question_reaches_the_executor_prompt(
    app_ctx, monkeypatch
) -> None:
    """Spec "A recall question is answered from the same prompt that
    writes the reply": with `{snack: 鱿鱼丝}` stored, the executor prompt
    for "我爱吃什么" carries the block, with no restatement by the user."""
    import agent.executor as executor_module

    await app_ctx.state.prefs_store.upsert_pref(42, "snack", "鱿鱼丝")
    monkeypatch.setattr(executor_module, "ChatOpenAI", _CapturingExecutorLlm)
    _CapturingExecutorLlm.reset()
    _StubLlm.call_count = 0

    r = await _post_chat(
        app_ctx, {"messages": [{"role": "user", "content": "我爱吃什么"}]}
    )
    assert r.status_code == 200
    assert "event: terminator" in r.text

    system_messages = [
        m.content for msgs in _CapturingExecutorLlm.captured for m in msgs if m.type == "system"
    ]
    assert system_messages, "the executor ran"
    assert any("- snack" in content and "鱿鱼丝" in content for content in system_messages)


async def test_no_stored_prefs_keeps_the_executor_prompt_untouched(
    app_ctx, monkeypatch
) -> None:
    import agent.executor as executor_module

    monkeypatch.setattr(executor_module, "ChatOpenAI", _CapturingExecutorLlm)
    _CapturingExecutorLlm.reset()
    _StubLlm.call_count = 0

    r = await _post_chat(
        app_ctx, {"messages": [{"role": "user", "content": "今天天气怎么样"}]}
    )
    assert r.status_code == 200
    assert "event: terminator" in r.text

    system_messages = [
        m.content for msgs in _CapturingExecutorLlm.captured for m in msgs if m.type == "system"
    ]
    assert system_messages == ["answer"], (
        "zero prefs must leave the executor prompt byte-identical"
    )


async def test_kill_switch_omits_the_block_on_the_executor_path(
    app_ctx_prefs_disabled, monkeypatch
) -> None:
    """Spec "Kill switch disables extraction and injection together":
    even with a stored row, `PYTHON_AGENT_PREFS_ENABLED=false` leaves the
    executor prompt untouched (the row stays in the table)."""
    import agent.executor as executor_module

    app = app_ctx_prefs_disabled
    await app.state.prefs_store.upsert_pref(42, "snack", "鱿鱼丝")
    monkeypatch.setattr(executor_module, "ChatOpenAI", _CapturingExecutorLlm)
    _CapturingExecutorLlm.reset()
    _StubLlm.call_count = 0

    r = await _post_chat(
        app, {"messages": [{"role": "user", "content": "我爱吃什么"}]}
    )
    assert r.status_code == 200
    assert "event: terminator" in r.text

    system_messages = [
        m.content for msgs in _CapturingExecutorLlm.captured for m in msgs if m.type == "system"
    ]
    assert system_messages == ["answer"], (
        "the kill switch must omit the block from the executor prompt"
    )
    assert len(await _stored_prefs(app)) == 1, "the stored row is untouched"
