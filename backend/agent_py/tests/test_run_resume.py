"""End-to-end resume test: kill mid-execution, drive a fresh graph against the
same `thread_id`, assert the planner doesn't re-run and the executor picks
up where it left off.

Gated on `TEST_ASYNC_PG_DSN` (self-skips without a live Postgres).

This is the contract the resume SSE path (`note{kind:"resumed"}`) depends
on. A restart of the Python service must not double-spend tokens on
already-completed steps.
"""
from __future__ import annotations

import asyncio
import json
import os
import uuid
from typing import Any, ClassVar

import asyncpg
import pytest

_DSN = os.environ.get("TEST_ASYNC_PG_DSN")

pytestmark = pytest.mark.skipif(
    not _DSN,
    reason="TEST_ASYNC_PG_DSN not set; needs a live Postgres (see Makefile / CI)",
)


class _StubStructuredChain:
    """See test_run.py — chainable shim whose `ainvoke` yields the canned Plan."""

    def __init__(self, plan_payload: dict):
        self._payload = plan_payload

    def __or__(self, other):
        return self

    async def ainvoke(self, messages, config=None):
        from agent.types import Plan

        return Plan.model_validate(self._payload)


class _StubLlm:
    """Shared counter. Phase 1 planner→plan; phase 1 executor llm→hang.
    Phase 2 planner (if re-invoked)→plan; phase 2 executor llm→text.

    The executor's `except Exception` swallows ordinary exceptions
    (turning them into step_end{failed}); the only way to truly
    interrupt mid-execution is `asyncio.CancelledError` (which is a
    BaseException, not caught by `except Exception`). The phase-1
    llm step hangs on an Event; the test cancels the graph task
    while the executor is waiting on the Event. The cancelled-task
    signal raises CancelledError inside the await, which propagates
    out of the graph.astream generator.
    """

    call_count = 0  # class-level; shared across instances
    llm_gate: asyncio.Event | None = None  # set by the test to release phase 2
    _PLAN: ClassVar[dict[str, Any]] = {
        "steps": [
            {"id": "step_1", "kind": "tool", "tool": "list_feeds", "args": {}},
            {"id": "step_2", "kind": "llm", "prompt": "summarise"},
        ]
    }

    def __init__(self, *args, **kwargs):
        pass

    def bind(self, **kwargs):
        # Planner path (structured default-on): consume the call-1 slot,
        # as the legacy flow did, so the executor's first call is n == 2.
        type(self).call_count += 1
        return _StubStructuredChain(self._PLAN)

    async def ainvoke(self, messages, config=None):
        from langchain_core.messages import AIMessage

        type(self).call_count += 1
        n = type(self).call_count
        if n == 1:
            # Phase 1 planner: return a 2-step plan
            return AIMessage(content=json.dumps(self._PLAN))
        if n == 2:
            # Phase 1 executor llm step: wait on a gate; the test cancels
            # the graph task to simulate a mid-execution abort.
            if _StubLlm.llm_gate is not None:
                await _StubLlm.llm_gate.wait()
            return AIMessage(content="summary: no feeds")
        # Phase 2 onwards: return the executor's final text
        return AIMessage(content="summary: no feeds")


class _StubBudget:
    class _D:
        decision = "ok"

    async def check_and_increment(self, user_id, tokens, tracer):
        return self._D()


class _StubRag:
    async def retrieve(self, query):
        from agent.rag_client import RagResult

        return RagResult(context_block="", citations=[])


class _StubTools:
    call_count = 0  # class-level; how many times the tool was called

    async def list_tools(self):
        return [
            {
                "type": "function",
                "function": {
                    "name": "list_feeds",
                    "description": "List user's feeds",
                    "parameters": {"type": "object", "properties": {}},
                },
            }
        ]

    async def call(self, name, arguments):
        type(self).call_count += 1
        return {"items": [1, 2, 3]}


async def _drop_schemas() -> None:
    conn = await asyncpg.connect(dsn=_DSN)
    try:
        await conn.execute("DROP SCHEMA IF EXISTS langgraph CASCADE")
        await conn.execute("DROP SCHEMA IF EXISTS agent CASCADE")
    finally:
        await conn.close()


def _patch_common(monkeypatch):
    """Patch the LLM / RAG / tools / observability shared by both phases."""
    import agent.executor as executor_module
    import agent.observability as obs_module
    import agent.planner as planner_module
    import agent.rag_client as rag_module
    import agent.tools_client as tools_module

    # Reset the shared counter so each test gets a fresh sequence.
    _StubLlm.call_count = 0

    monkeypatch.setattr(planner_module, "ChatOpenAI", _StubLlm)
    monkeypatch.setattr(executor_module, "ChatOpenAI", _StubLlm)
    monkeypatch.setattr(rag_module, "RagClient", lambda *a, **kw: _StubRag())
    monkeypatch.setattr(tools_module, "ToolsClient", lambda *a, **kw: _StubTools())

    def _noop_setup(settings):
        from opentelemetry import trace
        from opentelemetry.sdk.trace import TracerProvider

        provider = TracerProvider()
        trace.set_tracer_provider(provider)
        return trace.get_tracer("nyx-agent-test")

    monkeypatch.setattr(obs_module, "setup_observability", _noop_setup)


def _settings():
    import agent.config as config_module

    return config_module.Settings(
        _env_file=None,  # hermetic: never read the developer's .env
        database_url=_DSN,
        llm_base_url="http://localhost:9999/v1",
        metrics_port=0,
        retention_enabled=False,
    )


def _patch_settings(monkeypatch, settings):
    """Point config AND agent.main at the test's Settings BEFORE any app/lifespan.

    Two hazards this closes (found 2026-10-14 while landing
    agent-stale-run-sweeper):

    1. `agent.main` does `from .config import get_settings` — a from-import
       copies the function *reference* at import time, so patching
       `config.get_settings` alone does not rebind main's copy.
    2. Patching inside the lifespan block (the old pattern in this file) is
       too late: the lifespan's migrations + checkpointer already resolved
       the AMBIENT settings (`.env` / env vars), so phase 1 ran on one
       database while phase 2 ran on the test DSN — the "resume" then read
       empty state from the wrong DB and the planner re-ran.
    """
    import agent.config as config_module
    import agent.main as main_module

    monkeypatch.setattr(config_module, "_settings", settings)
    monkeypatch.setattr(config_module, "get_settings", lambda: settings)
    monkeypatch.setattr(main_module, "get_settings", lambda: settings)


async def test_planner_does_not_rerun_on_resume(monkeypatch):
    """langGraph's `astream(config={"configurable":{"thread_id": X}})` rehydrates
    the last checkpoint. If the planner already wrote a checkpoint, the
    planner node is NOT re-invoked on the resumed run.

    Mechanism: phase 1 hangs the executor's llm step on an Event;
    the test cancels the graph task to simulate a mid-execution abort.
    CancelledError (a BaseException) is not swallowed by the
    executor's `except Exception`, so it propagates out of graph.astream.
    Phase 2 opens a fresh lifespan (new checkpointer, new graph) against
    the same DB, drives the same thread_id, and asserts the planner
    is not re-invoked.
    """
    import asyncio

    from agent.graph import build_graph
    from agent.state import new_state
    from agent.types import ChatMessage

    _patch_common(monkeypatch)
    await _drop_schemas()
    settings = _settings()
    _patch_settings(monkeypatch, settings)

    # --- Phase 1: drive a run; cancel mid-execution; checkpoint lands. ---
    from agent.main import create_app, lifespan

    app1 = create_app()
    async with lifespan(app1):
        graph1 = build_graph(
            settings=app1.state.settings,
            budget=_StubBudget(),
            rag=_StubRag(),
            tools=_StubTools(),
            checkpointer=app1.state.checkpointer,
        )

        run_id = f"resume-{uuid.uuid4()}"
        state = new_state(
            user_id=1,
            run_id=run_id,
            messages=[ChatMessage(role="user", content="hi")],
            model="stub-model",
        )
        config = {"configurable": {"thread_id": run_id}}

        # Park the executor's llm step on an Event so the graph task
        # blocks inside the executor; we then cancel the task to
        # simulate a process abort. The gate is never set in phase 1.
        _StubLlm.llm_gate = asyncio.Event()

        graph_task = asyncio.create_task(
            _drain(graph1, state, config)
        )
        # Wait until the executor's llm call is in flight.
        for _ in range(100):
            if _StubLlm.call_count == 2:
                break
            await asyncio.sleep(0.01)
        assert _StubLlm.call_count == 2, (
            f"planner + executor-llm should have started; call_count={_StubLlm.call_count}"
        )
        graph_task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await graph_task
        _StubLlm.llm_gate = None  # release phase 2

        # The pre-cancel state has the plan + step_start + tool_call +
        # tool_result; the llm step didn't complete. get_state() must
        # return this in-progress state.
        snapshot = await graph1.aget_state(config)
        assert snapshot is not None
        kinds = [e.kind for e in snapshot.values.get("events_so_far", [])]
        assert "plan" in kinds
        assert "tool_call" in kinds
        assert "tool_result" in kinds
        # The llm step never completed: no delta for "summary: no feeds".
        assert not any(
            (e.payload or {}).get("delta") == "summary: no feeds"
            for e in snapshot.values.get("events_so_far", [])
        ), "llm step must NOT have completed before the cancel"

    # --- Phase 2: fresh lifespan = fresh checkpointer; same thread_id ---
    # The new checkpointer opens a fresh connection pool but talks to the
    # same DB, so it sees the checkpoints phase 1 wrote. The planner
    # MUST NOT re-run on phase 2.
    from agent.main import create_app as create_app2
    from agent.main import lifespan as lifespan2

    # Same test-scoped patches are still active; re-assert for symmetry.
    _patch_settings(monkeypatch, settings)

    app2 = create_app2()
    async with lifespan2(app2):
        graph2 = build_graph(
            settings=app2.state.settings,
            budget=_StubBudget(),
            rag=_StubRag(),
            tools=_StubTools(),
            checkpointer=app2.state.checkpointer,
        )

        # Drain the resumed run. The stub LLM is now the class itself
        # (with call_count still at 2 from phase 1). Any planner call
        # would increment to 3 BEFORE the executor's llm call; if
        # planner does NOT re-run, the next call is 3 (executor llm)
        # and returns "summary: no feeds".
        # Drain with `None` — the pure-resume idiom: langgraph treats any
        # non-None `astream` input as a state UPDATE, so a full
        # `new_state()` (or even `{}`) would clobber the checkpointed
        # plan/step_outputs and re-run the planner + completed steps (the
        # resume double-spend bug fixed 2026-10-14 in `run_agent_stream`;
        # this test drives the graph directly).
        async for _ in graph2.astream(None, config=config, stream_mode="values"):
            pass

        # call_count == 3 means: phase 1 used 2 (planner + executor llm
        # that was cancelled); phase 2 used 1 (executor llm that succeeded).
        # If the planner re-ran, call_count would be 4.
        assert _StubLlm.call_count == 3, (
            f"planner MUST NOT re-run on resume; expected call_count=3 "
            f"(2 from phase 1 + 1 resumed executor llm), got {_StubLlm.call_count}"
        )

        # The phase-2 executor completed without re-invoking the
        # planner. The detailed events_so_far contents for the resumed
        # run are covered by test_executor_skips_completed_steps_on_resume
        # and test_graph_persistence::test_get_state_round_trips_after_run;
        # this test focuses on the planner-resume contract via call_count.

    await _drop_schemas()


async def _drain(graph, state, config):
    """Helper: drain a graph into a list. Lets the test cancel the task."""
    out = []
    async for chunk in graph.astream(state, config=config, stream_mode="values"):
        out.append(chunk)
    return out


async def test_executor_skips_completed_steps_on_resume(monkeypatch):
    """Spec contract: 'prior steps are not re-executed (token usage
    confirms no double-spend)'.

    Pin the executor's behavior on resume: when the executor node is
    re-invoked against a thread_id whose checkpoint already has
    `step_outputs[step_1]`, the executor MUST NOT re-call the tool
    for step_1. Verified by tracking _StubTools.call_count across
    phases; phase 1's tool call increments it once, phase 2 must
    leave it at zero.
    """
    import asyncio

    from agent.graph import build_graph
    from agent.state import new_state
    from agent.types import ChatMessage

    _patch_common(monkeypatch)
    await _drop_schemas()
    settings = _settings()
    _patch_settings(monkeypatch, settings)

    from agent.main import create_app, lifespan

    app1 = create_app()
    async with lifespan(app1):
        graph1 = build_graph(
            settings=app1.state.settings,
            budget=_StubBudget(),
            rag=_StubRag(),
            tools=_StubTools(),
            checkpointer=app1.state.checkpointer,
        )

        run_id = f"resume-{uuid.uuid4()}"
        config = {"configurable": {"thread_id": run_id}}
        state = new_state(
            user_id=1,
            run_id=run_id,
            messages=[ChatMessage(role="user", content="hi")],
            model="stub-model",
        )

        # Park the executor's llm step on an Event; cancel mid-step.
        _StubLlm.llm_gate = asyncio.Event()
        graph_task = asyncio.create_task(_drain(graph1, state, config))
        for _ in range(100):
            if _StubLlm.call_count == 2:
                break
            await asyncio.sleep(0.01)
        assert _StubLlm.call_count == 2
        graph_task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await graph_task
        _StubLlm.llm_gate = None

        # step_1 (tool) is in step_outputs; step_2 (llm) was never run.
        # Reset the tools counter for phase 2.
        _StubTools.call_count = 0

    from agent.main import create_app as create_app2
    from agent.main import lifespan as lifespan2

    # Same test-scoped patches are still active; re-assert for symmetry.
    _patch_settings(monkeypatch, settings)

    app2 = create_app2()
    async with lifespan2(app2):
        # Reset LLM counter; phase 2's executor llm is the next call.
        _StubLlm.call_count = 2

        graph2 = build_graph(
            settings=app2.state.settings,
            budget=_StubBudget(),
            rag=_StubRag(),
            tools=_StubTools(),  # uses the counter-patched tools class
            checkpointer=app2.state.checkpointer,
        )

        # `None` input — resume must read state from the checkpoint, not
        # from a clobbering `new_state()` input (see test 1's note).
        async for _ in graph2.astream(None, config=config, stream_mode="values"):
            pass

        # EXPECTED (per spec): step_1 is skipped because step_outputs
        # already has it; the tools stub is NOT called again.
        # ACTUAL (current bug): the executor's loop iterates plan.steps
        # from the top; the tools stub is called again. This assertion
        # fails until the executor grows a resume-aware loop.
        assert _StubTools.call_count == 0, (
            f"executor re-ran the tool step on resume; expected 0 calls "
            f"(step_1 already in step_outputs), got {_StubTools.call_count}. "
            f"The executor needs to skip steps with populated step_outputs."
        )

    await _drop_schemas()
