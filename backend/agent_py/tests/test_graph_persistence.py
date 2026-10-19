"""End-to-end persistence test: compiled graph carries `AsyncPostgresSaver`,
per-node transitions land checkpoint rows in `langgraph.checkpoints`,
`graph.get_state(config)` round-trips the last state.

Companion to `test_smoke_run_state.py` — that file pins schema
placement (schemas land in the right place); this one pins the
per-node persistence contract (the saver actually writes and the
state is rehydratable).

Gated on `TEST_ASYNC_PG_DSN` (self-skips without a live Postgres).
"""
from __future__ import annotations

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
    """Shim for the planner's bound chain (structured path, default-on):
    `bind(response_format=...) | JsonOutputParser() | RunnableLambda(...)`.
    `__or__` chaining is ignored; `ainvoke` returns the canned plan
    validated into `Plan` — what the real chain produces
    (openspec/changes/agent-planner-structured)."""

    def __init__(self, plan_payload: dict):
        self._payload = plan_payload

    def __or__(self, other):
        return self

    async def ainvoke(self, messages, config=None):
        from agent.types import Plan

        return Plan.model_validate(self._payload)


class _StubLlm:
    """1-step plan on the first planner call, a final text on the executor call.

    The planner reaches the LLM via `bind(response_format=...)` (structured
    path, default-on); the executor uses plain `ainvoke`. Both consume the
    same canned plan, so the call sequence stays: call 1 = plan, call 2+ =
    executor text. Class-level `call_count` is shared across instances —
    the planner and the executor each construct their own instance.
    """

    call_count = 0  # class-level; shared across instances
    _PLAN: ClassVar[dict[str, Any]] = {
        "steps": [
            {"id": "step_1", "kind": "tool", "tool": "list_feeds", "args": {}},
            {"id": "step_2", "kind": "llm", "prompt": "summarise"},
        ]
    }

    def __init__(self, *args, **kwargs):
        pass

    def bind(self, **kwargs):
        # Planner path (structured, default-on): consume the call-1 slot as
        # the legacy flow did, so the executor's answering call is n == 2.
        type(self).call_count += 1
        return _StubStructuredChain(self._PLAN)

    async def ainvoke(self, messages, config=None):
        from langchain_core.messages import AIMessage

        type(self).call_count += 1
        n = type(self).call_count
        if n == 1:
            return AIMessage(content=json.dumps(self._PLAN))
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
    async def list_tools(self):
        # OpenAI-format tool schemas so bind_tools() succeeds.
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
        return {"items": [1, 2, 3]}


async def _drop_schemas() -> None:
    conn = await asyncpg.connect(dsn=_DSN)
    try:
        await conn.execute("DROP SCHEMA IF EXISTS langgraph CASCADE")
        await conn.execute("DROP SCHEMA IF EXISTS agent CASCADE")
    finally:
        await conn.close()


async def _count_checkpoints(thread_id: str) -> int:
    conn = await asyncpg.connect(dsn=_DSN)
    try:
        # langGraph stores the thread_id in `thread_id` (text) and the
        # binary blob in `checkpoint`. The history is shaped as a
        # JSON list under `checkpoint`; we count rows by thread_id.
        row = await conn.fetchrow(
            "SELECT count(*)::int AS n FROM langgraph.checkpoints WHERE thread_id = $1",
            thread_id,
        )
        return row["n"] if row else 0
    finally:
        await conn.close()


@pytest.fixture
async def app_with_checkpointer(monkeypatch):
    """Drive the real lifespan so the checkpointer is open and `setup()` has run."""
    _StubLlm.call_count = 0  # reset shared counter between tests
    await _drop_schemas()

    import agent.config as config_module

    settings = config_module.Settings(
        _env_file=None,  # hermetic: never read the developer's .env
        database_url=_DSN,
        llm_base_url="http://localhost:9999/v1",
        metrics_port=0,
        retention_enabled=False,
    )
    monkeypatch.setattr(config_module, "_settings", settings)
    monkeypatch.setattr(config_module, "get_settings", lambda: settings)

    # Stub LLM / RAG / tools so the planner + executor don't reach the wire.
    import agent.executor as executor_module
    import agent.planner as planner_module
    import agent.rag_client as rag_module
    import agent.tools_client as tools_module

    monkeypatch.setattr(planner_module, "ChatOpenAI", _StubLlm)
    monkeypatch.setattr(executor_module, "ChatOpenAI", _StubLlm)
    monkeypatch.setattr(rag_module, "RagClient", lambda *a, **kw: _StubRag())
    monkeypatch.setattr(tools_module, "ToolsClient", lambda *a, **kw: _StubTools())

    # Short-circuit OTel export — no Jaeger in test env.
    from agent import observability as obs_module

    def _noop_setup(settings):
        from opentelemetry import trace
        from opentelemetry.sdk.trace import TracerProvider

        provider = TracerProvider()
        trace.set_tracer_provider(provider)
        return trace.get_tracer("nyx-agent-test")

    monkeypatch.setattr(obs_module, "setup_observability", _noop_setup)

    from agent.main import create_app, lifespan

    app = create_app()
    async with lifespan(app):
        yield app


async def test_compiled_graph_carries_async_postgres_saver(app_with_checkpointer):
    """`build_graph(checkpointer=...)` produces a StateGraph whose compiled
    checkpointer is an `AsyncPostgresSaver`. Without this, no checkpoint
    row would ever be written — the smoke would pass but persistence
    would silently no-op.
    """
    from langgraph.checkpoint.postgres.aio import AsyncPostgresSaver

    from agent.graph import build_graph

    app = app_with_checkpointer
    assert isinstance(app.state.checkpointer, AsyncPostgresSaver), (
        "lifespan must wire an AsyncPostgresSaver onto app.state.checkpointer"
    )

    # Build the same graph shape the app uses, with the real checkpointer.
    graph = build_graph(
        settings=app.state.settings,
        budget=_StubBudget(),
        rag=_StubRag(),
        tools=_StubTools(),
        checkpointer=app.state.checkpointer,
    )
    # The compiled graph exposes its checkpointer via the public API.
    assert isinstance(graph.checkpointer, AsyncPostgresSaver)


async def test_node_transitions_land_checkpoint_rows(app_with_checkpointer):
    """Driving the graph for a fresh thread_id MUST produce at least one
    row in `langgraph.checkpoints` for that thread. Without persistence,
    a restart would lose the run.
    """
    from agent.graph import build_graph
    from agent.state import new_state
    from agent.types import ChatMessage

    app = app_with_checkpointer
    graph = build_graph(
        settings=app.state.settings,
        budget=_StubBudget(),
        rag=_StubRag(),
        tools=_StubTools(),
        checkpointer=app.state.checkpointer,
    )

    run_id = f"test-{uuid.uuid4()}"
    state = new_state(
        user_id=1,
        run_id=run_id,
        messages=[ChatMessage(role="user", content="hi")],
        model="stub-model",
    )
    config = {"configurable": {"thread_id": run_id}}

    # Drain the graph; with a checkpointer, every node transition writes
    # a checkpoint row for thread_id=run_id.
    async for _ in graph.astream(state, config=config, stream_mode="values"):
        pass

    rows = await _count_checkpoints(run_id)
    assert rows >= 1, (
        f"expected at least one checkpoint row for thread_id={run_id}, got {rows}; "
        "the saver didn't write — either the checkpointer isn't actually wired "
        "or setup() didn't land the tables"
    )


async def test_get_state_round_trips_after_run(app_with_checkpointer):
    """`graph.get_state(config)` returns the post-execution state for a
    thread that has been run. This is the contract the resume path
    depends on: a restarted process rehydrates `AgentState` from the
    last checkpoint and continues from the next node.
    """
    from agent.graph import build_graph
    from agent.state import new_state
    from agent.types import ChatMessage

    app = app_with_checkpointer
    graph = build_graph(
        settings=app.state.settings,
        budget=_StubBudget(),
        rag=_StubRag(),
        tools=_StubTools(),
        checkpointer=app.state.checkpointer,
    )

    run_id = f"test-{uuid.uuid4()}"
    state = new_state(
        user_id=1,
        run_id=run_id,
        messages=[ChatMessage(role="user", content="hi")],
        model="stub-model",
    )
    config = {"configurable": {"thread_id": run_id}}

    # Drive the graph; events_so_far must accumulate plan + tool_call + delta
    # frames the way the SSE stream would emit them.
    async for _ in graph.astream(state, config=config, stream_mode="values"):
        pass

    # Resume contract: a fresh get_state() call on the same thread_id
    # returns the last checkpointed state.
    snapshot = await graph.aget_state(config)
    assert snapshot is not None, "get_state must return a snapshot after a run"
    values = snapshot.values
    assert values["run_id"] == run_id, "snapshot must echo the run_id we ran with"
    events = values.get("events_so_far", [])
    assert events, "snapshot's events_so_far must not be empty after a run"
    # The planner emits a `plan` frame first; the executor emits at least
    # one tool_call / step_start / delta frame. The stub LLM returns
    # "summary: no feeds" for the executor's answering llm step.
    kinds = [e.kind for e in events]
    assert "plan" in kinds, f"plan frame missing from snapshot: {kinds}"
    assert "delta" in kinds, f"delta frame missing from snapshot: {kinds}"
    # And the delta carries the stub text (the answering step's content).
    deltas = [
        e.payload.get("delta")
        for e in events
        if e.kind == "delta" and e.payload.get("delta")
    ]
    assert any("summary: no feeds" in d for d in deltas), (
        f"stub executor text not in deltas: {deltas}"
    )
