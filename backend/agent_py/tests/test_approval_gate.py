"""Confirmation gate in the shared dispatch funnel — openspec/changes/
agent-feed-tools tasks 4.2/4.3 (core).

Drives the REAL `_dispatch_tool_with_retries` and `_run_react` with a real
`ToolDispatcher` over a stub MCP registry whose per-tool annotations drive
the gate (D3/D5/D6), plus the RunSink delivery-order contract and a
brokered-decision round-trip against a real (fakeredis-backed) store.
"""

from __future__ import annotations

import asyncio
import time
from typing import ClassVar

import fakeredis.aioredis
import pytest
from langchain_core.messages import AIMessage
from opentelemetry import trace

from agent import approvals as approvals_module
from agent import executor as executor_module
from agent.approvals import (
    DECISION_APPROVE,
    DECISION_DENY,
    DECISION_EXPIRED,
    ApprovalGate,
    ApprovalStore,
    RunSink,
    denial_text_for,
)
from agent.config import Settings
from agent.executor import _dispatch_tool_with_retries, _run_react
from agent.state import new_state
from agent.tool_dispatch import LocalToolRegistry, ToolDispatcher
from agent.tools_client import ToolError, ToolUnavailableError
from agent.types import Event, Plan, PlanStep, StepKind, StepStartEvent

STEP = "s1"


def _native(name: str, read_only: bool) -> dict:
    return {
        "name": name,
        "description": "does things",
        "inputSchema": {
            "type": "object",
            "properties": {"id": {"type": "integer"}},
        },
        "annotations": {"readOnlyHint": read_only},
    }


class _StubMcp:
    def __init__(self, tools=None):
        self.tools = (
            tools
            if tools is not None
            else [_native("create_feed", False), _native("list_feeds", True)]
        )
        self.calls: list[tuple[str, dict]] = []
        self.failures: list[Exception] = []  # popped per call, raised first

    async def list_tools(self):
        return list(self.tools)

    async def call(self, name, arguments):
        if self.failures:
            raise self.failures.pop(0)
        self.calls.append((name, arguments))
        return {"ok": True}


class _ScriptedStore:
    """Decision store double: scripted outcome, records intents/waits."""

    def __init__(self, decision=DECISION_APPROVE, put_error: Exception | None = None):
        self.decision = decision
        self.put_error = put_error
        self.intents: list[tuple[str, str, float]] = []
        self.waits = 0

    async def put_intent(self, run_id, approval_id, expires_at):
        if self.put_error is not None:
            raise self.put_error
        self.intents.append((run_id, approval_id, expires_at))

    async def wait_for_decision(self, run_id, approval_id, deadline):
        self.waits += 1
        return self.decision


class _ScriptedLlm:
    """Stand-in for ChatOpenAI: returns scripted AIMessages in order."""

    responses: ClassVar[list] = []

    def __init__(self, *args, **kwargs):
        pass

    def bind_tools(self, schemas):
        return self

    async def ainvoke(self, messages):
        return type(self).responses.pop(0)


class _StubBudget:
    class _D:
        decision = "ok"

    async def check_and_increment(self, user_id, tokens, tracer):
        return self._D()


def _settings() -> Settings:
    return Settings(
        _env_file=None,
        llm_base_url="http://stub",
        metrics_port=0,
        tool_retry_max=2,
        tool_retry_backoff_base_ms=0,
    )


def _state(run_id="r1"):
    return new_state(user_id=1, run_id=run_id, messages=[], model="stub")


def _dispatcher(mcp: _StubMcp) -> ToolDispatcher:
    return ToolDispatcher(mcp=mcp, local=LocalToolRegistry())


def _frames(state, kind: str) -> list:
    return [e for e in state["events_so_far"] if e.kind == kind]


def _gate(decision=DECISION_APPROVE, put_error=None, sink=None) -> ApprovalGate:
    return ApprovalGate(
        store=_ScriptedStore(decision, put_error), ttl_seconds=300, sink=sink
    )


@pytest.fixture
def tracer():
    return trace.get_tracer("test-approval-gate")


@pytest.fixture(autouse=True)
def _patch_llm(monkeypatch):
    monkeypatch.setattr(executor_module, "ChatOpenAI", _ScriptedLlm)
    _ScriptedLlm.responses = []
    yield


@pytest.fixture(autouse=True)
def _fast_poll(monkeypatch):
    monkeypatch.setattr(approvals_module, "_POLL_INTERVAL_SECONDS", 0.01)


async def _dispatch(tools, state, *, name, args=None, approvals=None, tracer, **extra):
    with tracer.start_as_current_span("x") as span:
        return await _dispatch_tool_with_retries(
            state,
            step_id=STEP,
            tool_name=name,
            args=args or {},
            tools=tools,
            approvals=approvals,
            settings=_settings(),
            tracer=tracer,
            budget=_StubBudget(),
            span=span,
            **extra,
        )


# --- 4.2 gate branches --------------------------------------------------------


async def test_read_only_registry_call_is_ungated(tracer):
    """Spec scenario: `list_feeds` (annotated read-only) dispatches without
    any confirmation — the gate must not even consult the store."""
    mcp = _StubMcp()
    gate = _gate(decision=DECISION_DENY)  # would deny if consulted
    state = _state()

    ok, result, _ = await _dispatch(
        _dispatcher(mcp), state, name="list_feeds", approvals=gate, tracer=tracer
    )

    assert ok is True and result == {"ok": True}
    assert _frames(state, "confirm_request") == []
    assert gate._store.intents == [] and gate._store.waits == 0
    assert mcp.calls == [("list_feeds", {})]


async def test_mutating_call_confirms_then_dispatches_on_approve(tracer):
    """Spec scenarios: the confirm frame carries the required fields, and an
    approve dispatches the original call with its regular frames in order."""
    mcp = _StubMcp()
    gate = _gate()
    state = _state()
    args = {"name": "Big Shit"}

    ok, _result, _ = await _dispatch(
        _dispatcher(mcp),
        state,
        name="create_feed",
        args=args,
        approvals=gate,
        tracer=tracer,
    )

    assert ok is True and mcp.calls == [("create_feed", args)]
    confirm = _frames(state, "confirm_request")
    assert len(confirm) == 1
    payload = confirm[0].payload
    assert payload["run_id"] == "r1"
    assert payload["step_id"] == STEP
    assert payload["tool"] == "create_feed"
    assert payload["args"] == args
    assert isinstance(payload["approval_id"], str) and payload["approval_id"]
    assert payload["expires_at"] == pytest.approx(time.time() + 300, abs=10)

    assert len(gate._store.intents) == 1
    kinds = [e.kind for e in state["events_so_far"]]
    assert kinds.index("confirm_request") < kinds.index("tool_call") < kinds.index(
        "tool_result"
    )
    assert _frames(state, "tool_result")[-1].payload["ok"] is True


async def test_deny_closes_call_model_visibly_without_dispatch(tracer):
    """Spec scenario: deny leaves data untouched (no registry call), the
    model gets a denial result, and no retry-counter budget is consumed."""
    mcp = _StubMcp()
    gate = _gate(decision=DECISION_DENY)
    state = _state()

    ok, result, content = await _dispatch(
        _dispatcher(mcp),
        state,
        name="create_feed",
        args={"name": "x"},
        approvals=gate,
        tracer=tracer,
    )

    assert ok is False and result is None
    assert content == denial_text_for(DECISION_DENY, "create_feed")
    assert mcp.calls == []  # never dispatched
    assert _frames(state, "tool_call") == []  # no regular call frames
    results = _frames(state, "tool_result")
    assert len(results) == 1
    assert results[0].payload["ok"] is False
    assert results[0].payload["error"] == content
    assert results[0].payload["retry_count"] == 0
    # Retry budget untouched; not a terminal failure record either.
    assert state["tool_retry_used"].get(STEP, {}) == {}
    assert STEP not in state.get("tool_terminal_failures", {})


async def test_expiry_is_a_denial_not_a_retry(tracer):
    mcp = _StubMcp()
    state = _state()
    ok, _result, content = await _dispatch(
        _dispatcher(mcp),
        state,
        name="create_feed",
        approvals=_gate(decision=DECISION_EXPIRED),
        tracer=tracer,
    )
    assert ok is False
    assert "expired" in content
    assert mcp.calls == []


async def test_store_unavailable_at_pause_blocks_the_call(tracer):
    """Broker-outage fail-closed: no intent, no confirm frame, no dispatch."""
    mcp = _StubMcp()
    state = _state()
    ok, _result, content = await _dispatch(
        _dispatcher(mcp),
        state,
        name="create_feed",
        approvals=_gate(put_error=RuntimeError("redis down")),
        tracer=tracer,
    )
    assert ok is False
    assert "unavailable" in content
    assert _frames(state, "confirm_request") == []
    assert mcp.calls == []


async def test_missing_gate_fails_closed(tracer):
    """No gate configured + mutating call ⇒ unavailable denial, never a
    silent dispatch (production always wires one; this pins fail-closed)."""
    mcp = _StubMcp()
    state = _state()
    ok, _result, content = await _dispatch(
        _dispatcher(mcp), state, name="create_feed", approvals=None, tracer=tracer
    )
    assert ok is False
    assert "unavailable" in content
    assert mcp.calls == []


async def test_approved_transient_retry_does_not_reconfirm(tracer):
    """The approval covers the intended mutation: a transient failure after
    approval retries the same call without a second confirmation."""
    mcp = _StubMcp()
    mcp.failures = [ToolUnavailableError("timeout")]
    gate = _gate()
    state = _state()

    ok, _result, _ = await _dispatch(
        _dispatcher(mcp),
        state,
        name="create_feed",
        args={"name": "x"},
        approvals=gate,
        tracer=tracer,
    )

    assert ok is True
    assert len(_frames(state, "confirm_request")) == 1
    assert gate._store.waits == 1
    assert mcp.calls == [("create_feed", {"name": "x"})]


async def test_corrected_redispatched_call_reconfirms(tracer):
    """An approval binds to the exact (tool, args) pair: after a registry
    rejection, the model's corrected call re-confirms before dispatch."""
    mcp = _StubMcp()
    mcp.failures = [ToolError("create_feed: name is required")]
    gate = _gate()
    state = _state()
    _ScriptedLlm.responses = [
        AIMessage(
            content="",
            tool_calls=[
                {"id": "c2", "name": "create_feed", "args": {"name": "good"}}
            ],
        )
    ]

    ok, _result, _ = await _dispatch(
        _dispatcher(mcp),
        state,
        name="create_feed",
        args={},
        approvals=gate,
        tracer=tracer,
        base_messages=[],
        step_prompt="create a feed",
        model_tool_call_id="c1",
    )

    assert ok is True
    assert len(_frames(state, "confirm_request")) == 2
    assert gate._store.waits == 2
    assert mcp.calls == [("create_feed", {"name": "good"})]


async def test_react_loop_mutating_call_gated_and_denied(tracer):
    """React loops pass the same gate (spec scenario); a denied call feeds
    the denial back as a ToolMessage and the loop continues to the final
    answer."""
    mcp = _StubMcp()
    gate = _gate(decision=DECISION_DENY)
    state = _state()
    _ScriptedLlm.responses = [
        AIMessage(
            content="",
            tool_calls=[{"id": "c1", "name": "create_feed", "args": {"name": "x"}}],
        ),
        AIMessage(content="done"),
    ]
    step = PlanStep(id="react_1", kind="react", prompt="p", max_iterations=2)
    state["plan"] = Plan(steps=[step])

    with tracer.start_as_current_span("x") as span:
        await _run_react(
            state,
            step,
            _settings(),
            tracer,
            _StubBudget(),
            _dispatcher(mcp),
            span,
            approvals=gate,
        )

    assert state["step_outputs"]["react_1"] == "done"
    assert mcp.calls == []
    confirm = _frames(state, "confirm_request")
    assert len(confirm) == 1 and confirm[0].payload["tool"] == "create_feed"
    denied = _frames(state, "tool_result")
    assert denied and denied[0].payload["ok"] is False


async def test_react_loop_approved_call_dispatches(tracer):
    mcp = _StubMcp()
    gate = _gate()
    state = _state()
    _ScriptedLlm.responses = [
        AIMessage(
            content="",
            tool_calls=[{"id": "c1", "name": "create_feed", "args": {"name": "x"}}],
        ),
        AIMessage(content="created"),
    ]
    step = PlanStep(id="react_1", kind="react", prompt="p", max_iterations=2)
    state["plan"] = Plan(steps=[step])

    with tracer.start_as_current_span("x") as span:
        await _run_react(
            state,
            step,
            _settings(),
            tracer,
            _StubBudget(),
            _dispatcher(mcp),
            span,
            approvals=gate,
        )

    assert mcp.calls == [("create_feed", {"name": "x"})]
    assert state["step_outputs"]["react_1"] == "created"


# --- 4.3 frame delivery (RunSink) --------------------------------------------


async def test_sink_flushes_prior_frames_before_confirm_request(tracer):
    """Wire order is preserved when the gate flushes mid-node: frames
    appended earlier in the node (step_start) reach the queue before the
    confirm prompt, and the confirm frame also lands in state for replay."""
    queue: asyncio.Queue = asyncio.Queue()
    sink = RunSink(queue)
    gate = _gate(sink=sink)
    state = _state()
    state["events_so_far"].append(
        Event(
            kind="step_start",
            payload=StepStartEvent(step_id=STEP, kind=StepKind.tool).model_dump(),
        )
    )

    ok, _result, _ = await _dispatch(
        _dispatcher(_StubMcp()),
        state,
        name="create_feed",
        args={"name": "x"},
        approvals=gate,
        tracer=tracer,
    )
    assert ok is True

    tag, first = queue.get_nowait()
    assert tag == "event" and first.kind == "step_start"
    tag, second = queue.get_nowait()
    assert tag == "event" and second.kind == "confirm_request"
    assert queue.empty()  # tool_call/tool_result ride the node-boundary flush
    assert len(_frames(state, "confirm_request")) == 1  # replay copy intact


async def test_run_sink_comment_and_close():
    queue: asyncio.Queue = asyncio.Queue()
    sink = RunSink(queue)
    await sink.comment("keep-alive")
    await sink.close()
    assert queue.get_nowait() == ("comment", "keep-alive")
    assert queue.get_nowait() is None


async def test_brokered_decision_resolves_the_waiting_dispatch(tracer):
    """Integration: real ApprovalStore over fakeredis — a decision recorded
    by "another replica" (any writer) resolves the waiting dispatch."""
    redis = fakeredis.aioredis.FakeRedis(decode_responses=True)
    store = ApprovalStore(redis)
    gate = ApprovalGate(store=store, ttl_seconds=5)
    mcp = _StubMcp()
    state = _state(run_id="run-x")

    task = asyncio.create_task(
        _dispatch(
            _dispatcher(mcp),
            state,
            name="create_feed",
            args={"name": "x"},
            approvals=gate,
            tracer=tracer,
        )
    )

    deadline = time.time() + 3
    while not _frames(state, "confirm_request") and time.time() < deadline:
        await asyncio.sleep(0.01)
    confirm = _frames(state, "confirm_request")
    assert confirm, "confirm_request frame never appeared"

    approval_id = confirm[0].payload["approval_id"]
    assert await store.record_decision("run-x", approval_id, DECISION_APPROVE) is True

    ok, _result, _ = await asyncio.wait_for(task, timeout=3)
    assert ok is True
    assert mcp.calls == [("create_feed", {"name": "x"})]
    await redis.aclose()


async def test_sink_survives_mid_list_insert_without_duplicates_or_loss():
    """Regression (review round 1): the executor inserts the partial-failure
    note MID-LIST — possibly at an index below what a mid-node gate flush
    already delivered. A positional delivery counter would re-emit the
    element at the stale index (the confirm_request — reviving a decided
    card) and drop the note. Identity-based delivery ships each event
    exactly once and still delivers the inserted note."""
    queue: asyncio.Queue = asyncio.Queue()
    sink = RunSink(queue)
    events = [
        Event(
            kind="step_start",
            payload=StepStartEvent(step_id=STEP, kind=StepKind.tool).model_dump(),
        ),
        Event(kind="delta", payload={"delta": "already streamed"}),
    ]
    await sink.flush(events)  # the gate's mid-node flush delivers both
    first_drain = []
    while not queue.empty():
        first_drain.append(queue.get_nowait())
    assert [item[1].kind for item in first_drain] == ["step_start", "delta"]

    note = Event(
        kind="note",
        payload={
            "text": "Run diverged from the plan",
            "kind": "partial_failure",
            "failed_steps": [STEP],
        },
    )
    events.insert(0, note)  # _note_insert_index picks an index below the delivered prefix
    events.append(Event(kind="done", payload={}))

    await sink.flush(events)

    drained = []
    while not queue.empty():
        drained.append(queue.get_nowait())
    assert [item[0] for item in drained] == ["event", "event"]
    # Exactly the two new events — nothing re-emitted, nothing lost.
    assert [item[1].kind for item in drained] == ["note", "done"]
    assert len(sink._delivered) == 4
