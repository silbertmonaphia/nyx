"""Planner registry catalog + graph wiring + sink-mode delivery —
openspec/changes/agent-feed-tools tasks 3.2/3.3 (and the graph half of 4.3).

The catalog block builder itself is covered in test_tool_normalization.py;
this file pins the WIRING: the block lands in the prompt, its absence is
byte-identical to the pre-change prompt, the planner node receives the
dispatcher, and `run_agent_stream`'s sink mode delivers frames (including
terminal frames) through the queue without yielding.
"""

from __future__ import annotations

import asyncio

import pytest
from langgraph.graph import END, START, StateGraph
from opentelemetry import trace

from agent import graph as graph_module
from agent import planner as planner_module
from agent.config import Settings
from agent.state import AgentState, new_state
from agent.tool_dispatch import registry_planner_block
from agent.types import Event

SETTINGS = Settings(_env_file=None, llm_base_url="http://stub", metrics_port=0)


def _fixture_tools() -> list[dict]:
    return [
        {
            "type": "function",
            "function": {
                "name": "list_feeds",
                "description": "List the user's feeds",
                "parameters": {},
            },
        },
        {
            "type": "function",
            "function": {
                "name": "create_feed",
                "description": "Create a feed",
                "parameters": {},
            },
        },
    ]


class _StubDispatcher:
    def __init__(self, tools: list[dict] | None = None):
        self._tools = tools if tools is not None else _fixture_tools()

    async def registry_tools(self) -> list[dict]:
        return list(self._tools)


class _FailingDispatcher:
    async def registry_tools(self):
        raise RuntimeError("registry down")


def _state() -> AgentState:
    return new_state(user_id=1, run_id="r1", messages=[], model="stub")


# --- 3.2: the prompt block ---------------------------------------------------


def test_catalog_block_appears_in_planner_prompt():
    block = registry_planner_block(_fixture_tools())
    msgs = planner_module._initial_messages(_state(), SETTINGS, "", block)
    system = msgs[0].content
    assert system.startswith(planner_module.PLANNER_SYSTEM_PROMPT)
    assert "Registry tools available" in system
    assert "- list_feeds: List the user's feeds" in system
    assert "- create_feed: Create a feed" in system


def test_prompt_byte_identical_when_catalog_absent():
    """Registry outage / empty list ⇒ the prompt equals the pre-change
    composition (spec scenario "Registry outage leaves planning unchanged")."""
    baseline = planner_module.PLANNER_SYSTEM_PROMPT + planner_module.planner_block(
        SETTINGS
    )
    for block in ("", registry_planner_block([])):
        msgs = planner_module._initial_messages(_state(), SETTINGS, "", block)
        assert msgs[0].content == baseline


async def test_plan_node_uses_dispatcher_catalog(monkeypatch):
    captured = {}

    async def _fake_emit(state, settings, tracer, prefs_text="", registry_block="", secrets=None):
        captured["block"] = registry_block
        return planner_module._PlanError("plan_invalid", "stop", "test")

    monkeypatch.setattr(planner_module, "_emit_structured_plan", _fake_emit)
    await planner_module.plan_node(
        _state(), SETTINGS, trace.get_tracer("t"), tools=_StubDispatcher()
    )
    assert "Registry tools available" in captured["block"]
    assert "- list_feeds" in captured["block"]


async def test_plan_node_catalog_fetch_failure_yields_empty_block(monkeypatch):
    captured = {}

    async def _fake_emit(state, settings, tracer, prefs_text="", registry_block="", secrets=None):
        captured["block"] = registry_block
        return planner_module._PlanError("plan_invalid", "stop", "test")

    monkeypatch.setattr(planner_module, "_emit_structured_plan", _fake_emit)
    await planner_module.plan_node(
        _state(), SETTINGS, trace.get_tracer("t"), tools=_FailingDispatcher()
    )
    assert captured["block"] == ""


async def test_plan_node_without_dispatcher_is_unchanged(monkeypatch):
    captured = {}

    async def _fake_emit(state, settings, tracer, prefs_text="", registry_block="", secrets=None):
        captured["block"] = registry_block
        return planner_module._PlanError("plan_invalid", "stop", "test")

    monkeypatch.setattr(planner_module, "_emit_structured_plan", _fake_emit)
    await planner_module.plan_node(_state(), SETTINGS, trace.get_tracer("t"))
    assert captured["block"] == ""


# --- 3.3: the graph factory threads the dispatcher ---------------------------


async def test_planner_factory_threads_dispatcher(monkeypatch):
    captured = {}

    async def _fake_plan_node(state, settings, tracer, prefs_text="", tools=None, secrets=None):
        captured["tools"] = tools
        return state

    monkeypatch.setattr(graph_module, "plan_node", _fake_plan_node)
    dispatcher = _StubDispatcher()
    node = graph_module._planner_step_factory(
        SETTINGS, trace.get_tracer("t"), "", dispatcher
    )
    await node(_state())
    assert captured["tools"] is dispatcher


# --- 4.3 (graph half): sink-mode delivery ------------------------------------


async def _delta_node(state: AgentState) -> AgentState:
    state["events_so_far"].append(Event(kind="delta", payload={"delta": "hi"}))
    state["terminal_status"] = "ok"
    return state


async def test_run_agent_stream_sink_mode_delivers_via_queue():
    from agent.approvals import RunSink

    queue: asyncio.Queue = asyncio.Queue()
    sink = RunSink(queue)

    builder = StateGraph(AgentState)
    builder.add_node("noop", _delta_node)
    builder.add_edge(START, "noop")
    builder.add_edge("noop", END)
    graph = builder.compile()

    state = new_state(user_id=1, run_id="r1", messages=[], model="stub")
    async for _ in graph_module.run_agent_stream(
        state,
        settings=SETTINGS,
        tracer=trace.get_tracer("t"),
        budget=object(),
        rag=object(),
        tools=object(),
        graph=graph,
        sink=sink,
    ):
        pytest.fail("sink mode must not yield")

    kinds = []
    while not queue.empty():
        tag, item = queue.get_nowait()
        assert tag == "event"
        kinds.append(item.kind)
    assert kinds == ["delta", "done", "terminator"]
    # Terminal frames also ride the state list in live mode (single path).
    assert [e.kind for e in state["events_so_far"]] == ["delta", "done", "terminator"]
