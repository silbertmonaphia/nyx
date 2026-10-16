"""Executor unit tests — data-step outputs threaded into answering steps.

`_render_prior_data` is the mechanism that lets a plan like [rag, llm]
actually see the retrieved feeds. Motivating bug (2026-10): "summarize
recent 3 months feed" planned a rag-only plan — and even with a
[rag, llm] plan the llm step's messages never included the retrieved
context_block because `step_outputs` was write-only.
"""

from __future__ import annotations

from typing import ClassVar

import pytest
from langchain_core.messages import AIMessage, ToolMessage
from opentelemetry import trace
from opentelemetry.sdk.trace import TracerProvider as _SdkTracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
from prometheus_client import REGISTRY

from agent import executor as executor_module
from agent.config import Settings
from agent.executor import (
    _collect_failed_steps,
    _compute_backoff_ms,
    _maybe_emit_partial_failure,
    _run_llm,
    _run_react,
    _run_tool,
)
from agent.main import _sse_encode
from agent.state import new_state
from agent.tools_client import ToolError, ToolRejectedError, ToolUnavailableError
from agent.types import (
    ChatMessage,
    DoneEvent,
    ErrorEvent,
    Event,
    NoteEvent,
    Plan,
    PlanEvent,
    PlanStep,
    RunIDEvent,
    StepEndEvent,
    TerminatorEvent,
    ToolResultEvent,
)


class _CapturingLlm:
    """Drop-in ChatOpenAI stub that records the message lists it receives."""

    captured: ClassVar[list] = []

    def __init__(self, *args, **kwargs):
        pass

    async def ainvoke(self, messages):
        type(self).captured.append(list(messages))
        return AIMessage(content="stub answer")


class _StubBudget:
    class _D:
        decision = "ok"

    async def check_and_increment(self, user_id, tokens, tracer):
        return self._D()


class _StubTools:
    async def list_tools(self):
        return []

    async def call(self, name, arguments):
        return {}


def _settings() -> Settings:
    # `tool_retry_*` pinned: env-independent budget, and a zero backoff base
    # so retry tests never actually sleep.
    return Settings(
        llm_base_url="http://stub",
        metrics_port=0,
        tool_retry_max=2,
        tool_retry_backoff_base_ms=0,
    )


def _state(plan_steps, step_outputs):
    state = new_state(
        user_id=1,
        run_id="test-run",
        messages=[ChatMessage(role="user", content="summarize my feeds")],
        model="stub-model",
    )
    state["plan"] = Plan(steps=plan_steps)
    state["step_outputs"] = step_outputs
    return state


@pytest.fixture(autouse=True)
def _patch_llm(monkeypatch):
    monkeypatch.setattr(executor_module, "ChatOpenAI", _CapturingLlm)
    _CapturingLlm.captured = []
    yield
    _CapturingLlm.captured = []


@pytest.fixture
def tracer():
    return trace.get_tracer("test-executor")


@pytest.mark.asyncio
async def test_llm_step_receives_prior_rag_context(tracer):
    steps = [
        PlanStep(id="rag_1", kind="rag", query="recent 3 months"),
        PlanStep(id="llm_2", kind="llm", prompt="Summarize the feeds."),
    ]
    state = _state(
        steps, {"rag_1": {"context_block": "FEEDDATA: 42 posts", "citations": []}}
    )

    with tracer.start_as_current_span("x") as span:
        await _run_llm(state, steps[1], _settings(), tracer, _StubBudget(), span)

    msgs = _CapturingLlm.captured[0]
    assert msgs[0].content == "Summarize the feeds."
    assert "FEEDDATA: 42 posts" in msgs[1].content
    assert "rag_1" in msgs[1].content
    # Conversation follows the two system messages.
    assert msgs[2].content == "summarize my feeds"
    # The answer still streams as a delta.
    deltas = [e for e in state["events_so_far"] if e.kind == "delta"]
    assert deltas and deltas[0].payload["delta"] == "stub answer"


@pytest.mark.asyncio
async def test_llm_step_without_data_steps_keeps_prompt_shape(tracer):
    steps = [PlanStep(id="llm_1", kind="llm", prompt="hi")]
    state = _state(steps, {})

    with tracer.start_as_current_span("x") as span:
        await _run_llm(state, steps[0], _settings(), tracer, _StubBudget(), span)

    msgs = _CapturingLlm.captured[0]
    # No injected context message: prompt, then conversation — the exact
    # prior shape for data-free plans (greetings).
    assert msgs[0].content == "hi"
    assert msgs[1].content == "summarize my feeds"
    assert len(msgs) == 2


@pytest.mark.asyncio
async def test_tool_result_is_included_in_llm_context(tracer):
    steps = [
        PlanStep(id="tool_1", kind="tool", tool="list_feeds", args={}),
        PlanStep(id="llm_2", kind="llm", prompt="Summarize."),
    ]
    state = _state(steps, {"tool_1": {"items": ["a", "b"]}})

    with tracer.start_as_current_span("x") as span:
        await _run_llm(state, steps[1], _settings(), tracer, _StubBudget(), span)

    msgs = _CapturingLlm.captured[0]
    assert '"items"' in msgs[1].content
    assert "list_feeds" in msgs[1].content


@pytest.mark.asyncio
async def test_executor_rejects_non_plan_state(tracer):
    """Oracle for the planner→executor type contract: the planner always
    emits a typed `Plan` (structured output); a dict here means the planner
    regressed to its legacy dict-only path — the executor must fail loudly
    instead of parsing garbage."""

    state = _state([], {})
    state["plan"] = {"steps": []}  # type: ignore[assignment]

    with pytest.raises(AssertionError, match="expected Plan"):
        await executor_module.executor_node(
            state,
            settings=_settings(),
            tracer=tracer,
            budget=_StubBudget(),
            rag=None,  # type: ignore[arg-type] — unused: assert fires first
            tools=None,  # type: ignore[arg-type]
        )


@pytest.mark.asyncio
async def test_react_step_receives_prior_rag_context(tracer):
    steps = [
        PlanStep(id="rag_1", kind="rag", query="q"),
        PlanStep(
            id="react_2", kind="react", prompt="Answer using the data.", max_iterations=1
        ),
    ]
    state = _state(steps, {"rag_1": {"context_block": "FEEDDATA", "citations": []}})

    with tracer.start_as_current_span("x") as span:
        await _run_react(
            state, steps[1], _settings(), tracer, _StubBudget(), _StubTools(), span
        )

    msgs = _CapturingLlm.captured[0]
    assert "FEEDDATA" in msgs[1].content


@pytest.mark.asyncio
async def test_react_step_without_prompt_gets_objective_and_stop_rule(tracer):
    """A react step whose planner omitted `prompt` (the shape the planner
    prompt historically advertised — `{tool, args, max_iterations}`) MUST
    NOT run against an empty system message.

    Motivating incident (2026-10-14): "summarize my recent 3 months of
    feeds, then post the summary as a new feed" planned exactly such a
    react step; with no objective and no stop rule the model re-called
    tools every iteration until the cap failed the step with
    "ReAct step exceeded iteration cap". The synthesized instruction
    supplies the objective AND the stop rule; the step's declared tool
    rides along as a hint.
    """
    step = PlanStep(id="react_1", kind="react", tool="list_feeds", max_iterations=3)
    state = _state([step], {})

    with tracer.start_as_current_span("x") as span:
        await _run_react(
            state, step, _settings(), tracer, _StubBudget(), _StubTools(), span
        )

    msgs = _CapturingLlm.captured[0]
    system = msgs[0].content
    assert system, "a react step must never reach the model with an empty system message"
    assert "final text" in system, "the objective must name the expected outcome"
    assert "WITHOUT calling any more tools" in system, (
        "the stop rule is load-bearing: without it a small model loops tool "
        "calls until the iteration cap"
    )
    assert "`list_feeds`" in system, "the step's declared tool is carried as a hint"
    # The text-only model reply ends the loop and becomes the step output.
    assert state["step_outputs"]["react_1"] == "stub answer"


@pytest.mark.asyncio
async def test_react_step_with_prompt_uses_it_verbatim(tracer):
    """A planner-supplied `prompt` is the loop's system instruction, byte
    for byte — no prefix/suffix is added (the tool hint only backfills a
    missing prompt)."""
    step = PlanStep(
        id="react_1",
        kind="react",
        tool="list_feeds",
        prompt="Answer using the data.",
        max_iterations=1,
    )
    state = _state([step], {})

    with tracer.start_as_current_span("x") as span:
        await _run_react(
            state, step, _settings(), tracer, _StubBudget(), _StubTools(), span
        )

    msgs = _CapturingLlm.captured[0]
    assert msgs[0].content == "Answer using the data."


# --- tool-dispatch retry (openspec/changes/agent-tool-dispatch-retry) -----


class _ScriptedTools:
    """ToolsClient stub with a scripted outcome per call.

    An outcome that is an Exception instance is raised; anything else is
    returned as the call result. The final outcome repeats once the script
    is down to one entry, so `[exc]` means "always fails".
    """

    def __init__(self, script, schemas=None):
        self.script = list(script)
        self.schemas = schemas or []
        self.calls = []

    async def list_tools(self):
        return self.schemas

    async def call(self, name, arguments):
        self.calls.append((name, arguments))
        outcome = self.script.pop(0) if len(self.script) > 1 else self.script[0]
        if isinstance(outcome, Exception):
            raise outcome
        return outcome


class _ScriptedLlm:
    """ChatOpenAI stub returning scripted AIMessages; records message lists.

    Repeat-last semantics, same as `_ScriptedTools`.
    """

    captured: ClassVar[list] = []
    responses: ClassVar[list] = []

    def __init__(self, *args, **kwargs):
        pass

    def bind_tools(self, schemas):
        return self

    async def ainvoke(self, messages):
        type(self).captured.append(list(messages))
        if not type(self).responses:
            return AIMessage(content="stub answer")
        return (
            type(self).responses.pop(0)
            if len(type(self).responses) > 1
            else type(self).responses[0]
        )


def _tool_call(name="list_feeds", args=None, tc_id="tc_1"):
    return AIMessage(
        content="", tool_calls=[{"id": tc_id, "name": name, "args": args or {}}]
    )


def _use_llm(monkeypatch, responses):
    monkeypatch.setattr(executor_module, "ChatOpenAI", _ScriptedLlm)
    _ScriptedLlm.captured = []
    _ScriptedLlm.responses = list(responses)


def _retry_counter(tool, reason):
    return (
        REGISTRY.get_sample_value(
            "agent_tool_retries_total", {"tool": tool, "reason": reason}
        )
        or 0.0
    )


def _tool_steps(*step_ids, tool="list_feeds"):
    return [PlanStep(id=sid, kind="tool", tool=tool, args={}) for sid in step_ids]


@pytest.mark.asyncio
async def test_executor_retries_on_registry_validation_failure(tracer, monkeypatch):
    """Spec: validator rejection → re-prompt the model, dispatch the
    corrected call, succeed with retry_count=1 (§3.3)."""
    steps = _tool_steps("tool_1")
    state = _state(steps, {})
    tools = _ScriptedTools(
        [ToolError("list_feeds: field limit: expected int, got str"), {"items": ["a"]}]
    )
    _use_llm(monkeypatch, [_tool_call(args={"limit": 5})])

    before = _retry_counter("list_feeds", "tool_arg_invalid")
    with tracer.start_as_current_span("x") as span:
        await _run_tool(
            state,
            steps[0],
            settings=_settings(),
            tracer=tracer,
            budget=_StubBudget(),
            tools=tools,
            span=span,
        )

    assert _retry_counter("list_feeds", "tool_arg_invalid") - before == 1
    # The retry dispatched the MODEL'S corrected call, not the same args.
    assert [c[1] for c in tools.calls] == [{}, {"limit": 5}]
    results = [e for e in state["events_so_far"] if e.kind == "tool_result"]
    assert results[0].payload["ok"] is False
    assert results[0].payload["retry_count"] == 0  # first-attempt failure
    assert results[-1].payload["ok"] is True
    assert results[-1].payload["retry_count"] == 1
    assert state["step_outputs"]["tool_1"] == {"items": ["a"]}


@pytest.mark.asyncio
async def test_executor_terminates_step_on_persistent_validation_failure(
    tracer, monkeypatch
):
    """Spec: persistent rejection → step fails after exactly `tool_retry_max`
    retries (§3.4); the terminal frame carries the validator's (safe) error."""
    steps = _tool_steps("tool_1")
    state = _state(steps, {})
    tools = _ScriptedTools([ToolError("list_feeds: missing required field operator")])
    _use_llm(monkeypatch, [_tool_call(args={"limit": 5})])

    before = _retry_counter("list_feeds", "tool_arg_invalid")
    with tracer.start_as_current_span("x") as span, pytest.raises(RuntimeError):
        await _run_tool(
            state,
            steps[0],
            settings=_settings(),
            tracer=tracer,
            budget=_StubBudget(),
            tools=tools,
            span=span,
        )

    # Exactly tool_retry_max increments — not retry_max + 1.
    assert _retry_counter("list_feeds", "tool_arg_invalid") - before == 2
    assert len(tools.calls) == 3  # first attempt + 2 retries
    final = [e for e in state["events_so_far"] if e.kind == "tool_result"][-1]
    assert final.payload["ok"] is False
    assert final.payload["retry_count"] == 2
    assert "missing required field" in final.payload["error"]


@pytest.mark.asyncio
async def test_per_step_budget_isolates_retry_state(tracer, monkeypatch):
    """Spec: exhaustion in one step does not consume sibling steps' budget
    (§3.5). Two failing tool steps each get a full budget."""
    steps = _tool_steps("tool_1", "tool_2")
    state = _state(steps, {})
    tools = _ScriptedTools([ToolError("list_feeds: bad args")])
    _use_llm(monkeypatch, [_tool_call(args={"limit": 5})])

    before = _retry_counter("list_feeds", "tool_arg_invalid")
    await executor_module.executor_node(
        state,
        settings=_settings(),
        tracer=tracer,
        budget=_StubBudget(),
        rag=None,  # type: ignore[arg-type] — no rag steps in this plan
        tools=tools,
    )

    assert _retry_counter("list_feeds", "tool_arg_invalid") - before == 4  # 2 × 2
    assert len(tools.calls) == 6  # 3 attempts per step
    step_ends = [e for e in state["events_so_far"] if e.kind == "step_end"]
    assert [e.payload["status"] for e in step_ends] == ["failed", "failed"]


@pytest.mark.asyncio
async def test_retry_reprompt_contains_validator_error(tracer, monkeypatch):
    """Spec: re-prompt structure — messages end with the assistant message
    that emitted the bad call, the rejected tool result, and a final user
    message quoting the validator's error + the tool's schema (§3.6)."""
    steps = _tool_steps("tool_1")
    state = _state(steps, {})
    schemas = [
        {
            "type": "function",
            "function": {
                "name": "list_feeds",
                "parameters": {
                    "type": "object",
                    "properties": {"limit": {"type": "integer"}},
                },
            },
        }
    ]
    validator_msg = "list_feeds: limit must be an integer"
    tools = _ScriptedTools([ToolError(validator_msg)], schemas=schemas)
    _use_llm(monkeypatch, [_tool_call(args={"limit": 5})])

    with tracer.start_as_current_span("x") as span, pytest.raises(RuntimeError):
        await _run_tool(
            state,
            steps[0],
            settings=_settings(),
            tracer=tracer,
            budget=_StubBudget(),
            tools=tools,
            span=span,
        )

    msgs = _ScriptedLlm.captured[0]
    final_user = msgs[-1]
    assert "PREVIOUS_ATTEMPT_FAILED" in final_user.content
    assert validator_msg in final_user.content
    assert '"limit"' in final_user.content  # argument schema included
    tool_msg = msgs[-2]
    assistant = msgs[-3]
    assert isinstance(tool_msg, ToolMessage)
    assert tool_msg.tool_call_id == assistant.tool_calls[0]["id"]
    assert assistant.tool_calls[0]["name"] == "list_feeds"
    # The original user request is carried over earlier in the list, not
    # re-quoted inside the re-prompt's final user message.
    assert any("summarize my feeds" in (m.content or "") for m in msgs[:-1])
    assert "summarize my feeds" not in final_user.content


@pytest.mark.asyncio
async def test_executor_retries_on_transient_failure(tracer):
    """Spec: transient failure → backoff retry of the same call; no model
    re-prompt on this path (§4.2)."""
    steps = _tool_steps("tool_1")
    state = _state(steps, {})
    tools = _ScriptedTools([ToolUnavailableError("MCP 503: overloaded"), {"items": []}])

    before = _retry_counter("list_feeds", "tool_execution_transient")
    with tracer.start_as_current_span("x") as span:
        await _run_tool(
            state,
            steps[0],
            settings=_settings(),
            tracer=tracer,
            budget=_StubBudget(),
            tools=tools,
            span=span,
        )

    assert _retry_counter("list_feeds", "tool_execution_transient") - before == 1
    # The transient path never re-invokes the model (autouse _CapturingLlm).
    assert _CapturingLlm.captured == []
    assert len(tools.calls) == 2
    final = [e for e in state["events_so_far"] if e.kind == "tool_result"][-1]
    assert final.payload["ok"] is True
    assert final.payload["retry_count"] == 1


@pytest.mark.asyncio
async def test_executor_terminates_step_on_persistent_transient_failure(tracer):
    """Spec: persistent 5xx → step fails after `tool_retry_max` retries; the
    wire carries static safe text, never the raw transport message (§4.3)."""
    steps = _tool_steps("tool_1")
    state = _state(steps, {})
    tools = _ScriptedTools(
        [ToolUnavailableError("MCP 503: upstream exploded: internal detail")]
    )

    before = _retry_counter("list_feeds", "tool_execution_transient")
    with tracer.start_as_current_span("x") as span, pytest.raises(RuntimeError):
        await _run_tool(
            state,
            steps[0],
            settings=_settings(),
            tracer=tracer,
            budget=_StubBudget(),
            tools=tools,
            span=span,
        )

    assert _retry_counter("list_feeds", "tool_execution_transient") - before == 2
    assert len(tools.calls) == 3
    final = [e for e in state["events_so_far"] if e.kind == "tool_result"][-1]
    assert final.payload["ok"] is False
    assert final.payload["retry_count"] == 2
    assert final.payload["error"] == "tool unavailable"
    assert "exploded" not in final.payload["error"]


@pytest.mark.asyncio
async def test_4xx_non_retryable_fails_fast(tracer):
    """Spec: a bare 4xx (no JSON-RPC envelope) is non-retryable — one
    attempt, no counter increment, immediate step failure (§4.4)."""
    steps = _tool_steps("tool_1")
    state = _state(steps, {})
    tools = _ScriptedTools(
        [ToolRejectedError("MCP 400: bad request", status_code=400)]
    )

    before_arg = _retry_counter("list_feeds", "tool_arg_invalid")
    before_transient = _retry_counter("list_feeds", "tool_execution_transient")
    with tracer.start_as_current_span("x") as span, pytest.raises(RuntimeError):
        await _run_tool(
            state,
            steps[0],
            settings=_settings(),
            tracer=tracer,
            budget=_StubBudget(),
            tools=tools,
            span=span,
        )

    assert len(tools.calls) == 1
    assert _retry_counter("list_feeds", "tool_arg_invalid") - before_arg == 0
    assert _retry_counter("list_feeds", "tool_execution_transient") - before_transient == 0
    final = [e for e in state["events_so_far"] if e.kind == "tool_result"][-1]
    assert final.payload["ok"] is False
    assert final.payload["retry_count"] == 0
    assert final.payload["error"] == "tool call rejected (HTTP 400)"


@pytest.mark.asyncio
async def test_validation_error_routes_to_arg_path_not_transient(tracer, monkeypatch):
    """Spec: a typed validation error MUST NOT trigger the transient retry
    loop even when it surfaces through the transport (§4.5)."""
    steps = _tool_steps("tool_1")
    state = _state(steps, {})
    tools = _ScriptedTools(
        [ToolError("list_feeds: MCP 422 validation failed: limit must be an integer"), {"items": []}]
    )
    _use_llm(monkeypatch, [_tool_call(args={"limit": 5})])

    before_arg = _retry_counter("list_feeds", "tool_arg_invalid")
    before_transient = _retry_counter("list_feeds", "tool_execution_transient")
    with tracer.start_as_current_span("x") as span:
        await _run_tool(
            state,
            steps[0],
            settings=_settings(),
            tracer=tracer,
            budget=_StubBudget(),
            tools=tools,
            span=span,
        )

    assert _retry_counter("list_feeds", "tool_arg_invalid") - before_arg == 1
    assert _retry_counter("list_feeds", "tool_execution_transient") - before_transient == 0
    assert len(_ScriptedLlm.captured) == 1  # routed via the model re-prompt
    assert [c[1] for c in tools.calls] == [{}, {"limit": 5}]  # corrected args, not same


@pytest.mark.asyncio
async def test_react_step_shares_budget_across_iterations(tracer, monkeypatch):
    """design.md §Open Questions (resolved 2026-10-13): the per-(step, tool)
    budget spans the whole ReAct step — iteration 2 does NOT refresh it.
    Iter 0 exhausts 2 retries (3 attempts); iter 1 fails fast (1 attempt)."""
    step = PlanStep(id="react_1", kind="react", prompt="p", max_iterations=3)
    state = _state([step], {})
    tools = _ScriptedTools([ToolError("list_feeds: bad args")])
    _use_llm(
        monkeypatch,
        [_tool_call(args={"limit": "x"})] * 4 + [AIMessage(content="done")],
    )

    before = _retry_counter("list_feeds", "tool_arg_invalid")
    with tracer.start_as_current_span("x") as span:
        await _run_react(state, step, _settings(), tracer, _StubBudget(), tools, span)

    assert len(tools.calls) == 4  # 3 attempts in iter 0 + 1 fail-fast in iter 1
    assert _retry_counter("list_feeds", "tool_arg_invalid") - before == 2
    results = [e for e in state["events_so_far"] if e.kind == "tool_result"]
    assert results[-1].payload["ok"] is False
    assert results[-1].payload["retry_count"] == 0  # fail-fast makes no retries


# --- partial-failure signal (spec: agent-partial-failure) -----------------


def test_collect_failed_steps_priority():
    """Spec scenario set for `_collect_failed_steps`: step-failed and
    terminal-tool-failure count; RAG-empty and run_metadata_unavailable do
    not; the intermediate retry failure does not."""
    state = _state([], {})
    state["events_so_far"].extend(
        [
            # RAG empty — legitimate no-results, not a failure.
            Event(
                kind="tool_result",
                payload=ToolResultEvent(
                    step_id="rag_1", call_id="c1", ok=True, result=[]
                ).model_dump(),
            ),
            Event(
                kind="step_end",
                payload=StepEndEvent(step_id="rag_1", status="ok").model_dump(),
            ),
            # Intermediate retry failure (retry_count < tool_retry_max).
            Event(
                kind="tool_result",
                payload=ToolResultEvent(
                    step_id="mid", call_id="c2", ok=False, error="x", retry_count=1
                ).model_dump(),
            ),
            # Terminal tool failure (retry_count == tool_retry_max).
            Event(
                kind="tool_result",
                payload=ToolResultEvent(
                    step_id="term_step", call_id="c3", ok=False, error="x", retry_count=2
                ).model_dump(),
            ),
            # Failed step.
            Event(
                kind="step_end",
                payload=StepEndEvent(step_id="fail_step", status="failed").model_dump(),
            ),
            # Non-fatal — explicitly excluded from the trigger.
            Event(
                kind="error",
                payload=ErrorEvent(
                    error="x", code="run_metadata_unavailable"
                ).model_dump(),
            ),
        ]
    )
    assert _collect_failed_steps(state, _settings()) == ["term_step", "fail_step"]


def test_no_banner_for_run_metadata_unavailable_with_all_steps_ok():
    """Spec: non-fatal run-metadata errors do not trigger the banner."""
    state = _state([], {})
    state["events_so_far"].extend(
        [
            Event(
                kind="step_end",
                payload=StepEndEvent(step_id="s1", status="ok").model_dump(),
            ),
            Event(
                kind="error",
                payload=ErrorEvent(
                    error="x", code="run_metadata_unavailable"
                ).model_dump(),
            ),
        ]
    )
    _maybe_emit_partial_failure(state, _settings())
    assert [e for e in state["events_so_far"] if e.kind == "note"] == []


def test_no_banner_for_rag_empty_result():
    """Spec: an empty retrieval is a valid plan outcome, not a failure."""
    state = _state([], {})
    state["events_so_far"].extend(
        [
            Event(
                kind="tool_result",
                payload=ToolResultEvent(
                    step_id="rag_1", call_id="c", ok=True, result=[]
                ).model_dump(),
            ),
            Event(
                kind="step_end",
                payload=StepEndEvent(step_id="rag_1", status="ok").model_dump(),
            ),
        ]
    )
    _maybe_emit_partial_failure(state, _settings())
    assert [e for e in state["events_so_far"] if e.kind == "note"] == []


def test_banner_with_empty_failed_steps_for_plan_too_large():
    """Spec: plan-level errors fire the signal with an empty step list, and
    the note is inserted before the already-present terminal `done` frame."""
    state = _state([], {})
    state["events_so_far"].extend(
        [
            Event(
                kind="error",
                payload=ErrorEvent(error="too many steps", code="plan_too_large").model_dump(),
            ),
            Event(kind="done", payload=DoneEvent().model_dump()),
        ]
    )
    _maybe_emit_partial_failure(state, _settings())
    assert [e.kind for e in state["events_so_far"]] == ["error", "note", "done"]
    note = state["events_so_far"][1]
    assert note.payload["kind"] == "partial_failure"
    assert note.payload["failed_steps"] == []


@pytest.mark.asyncio
async def test_partial_failure_emitted_for_tool_retry_exhaustion(tracer, monkeypatch):
    """Spec: exhaustion → a single note before the final answer's deltas,
    naming the failed step."""
    steps = [
        PlanStep(id="tool_1", kind="tool", tool="list_feeds", args={}),
        PlanStep(id="llm_2", kind="llm", prompt="Answer."),
    ]
    state = _state(steps, {})
    tools = _ScriptedTools([ToolError("list_feeds: bad args")])
    _use_llm(
        monkeypatch,
        [
            _tool_call(args={"limit": 5}),
            _tool_call(args={"limit": 5}),
            AIMessage(content="final answer"),
        ],
    )

    await executor_module.executor_node(
        state,
        settings=_settings(),
        tracer=tracer,
        budget=_StubBudget(),
        rag=None,  # type: ignore[arg-type] — no rag steps
        tools=tools,
    )

    events = state["events_so_far"]
    note_idx = [i for i, e in enumerate(events) if e.kind == "note"]
    assert len(note_idx) == 1
    note = events[note_idx[0]]
    assert note.payload["kind"] == "partial_failure"
    assert note.payload["failed_steps"] == ["tool_1"]
    final_delta_idx = max(
        i
        for i, e in enumerate(events)
        if e.kind == "delta" and e.payload.get("delta") == "final answer"
    )
    assert note_idx[0] < final_delta_idx  # before the final answer's deltas


@pytest.mark.asyncio
async def test_no_banner_for_intermediate_retry_failure_recovered(tracer, monkeypatch):
    """Spec: a retry that recovers within budget is not a partial failure."""
    steps = _tool_steps("tool_1")
    state = _state(steps, {})
    tools = _ScriptedTools([ToolError("list_feeds: bad args"), {"items": []}])
    _use_llm(monkeypatch, [_tool_call(args={"limit": 5})])

    await executor_module.executor_node(
        state,
        settings=_settings(),
        tracer=tracer,
        budget=_StubBudget(),
        rag=None,  # type: ignore[arg-type]
        tools=tools,
    )

    assert [e for e in state["events_so_far"] if e.kind == "note"] == []


@pytest.mark.asyncio
async def test_banner_aggregates_two_failed_steps_in_plan_order(tracer, monkeypatch):
    """Spec: multiple failures → one note listing every failed step id."""
    steps = _tool_steps("tool_1", "tool_2") + [
        PlanStep(id="llm_3", kind="llm", prompt="Answer.")
    ]
    state = _state(steps, {})
    tools = _ScriptedTools([ToolError("list_feeds: bad args")])
    _use_llm(
        monkeypatch,
        [_tool_call(args={"limit": 5})] * 4 + [AIMessage(content="final answer")],
    )

    await executor_module.executor_node(
        state,
        settings=_settings(),
        tracer=tracer,
        budget=_StubBudget(),
        rag=None,  # type: ignore[arg-type]
        tools=tools,
    )

    notes = [e for e in state["events_so_far"] if e.kind == "note"]
    assert len(notes) == 1
    assert notes[0].payload["failed_steps"] == ["tool_1", "tool_2"]


# --- cross-runtime wire fixtures ------------------------------------------
# Frozen SSE bytes; the identical strings are parsed by the TypeScript
# consumer test in `frontend/src/features/chat/services/chatService.test.ts`
# ("wire fixture" cases). Producer and consumer cannot drift silently —
# changing a frame shape breaks both sides (FUTURE_AGENT.md lessons).


def test_tool_result_wire_fixture_retry_count():
    ev = Event(
        kind="tool_result",
        payload=ToolResultEvent(
            step_id="step_1", call_id="c1", ok=True, result={"items": []}, retry_count=2
        ).model_dump(),
    )
    assert _sse_encode(ev) == (
        b"event: tool_result\n"
        b'data: {"step_id":"step_1","call_id":"c1","ok":true,'
        b'"result":{"items":[]},"error":null,"retry_count":2}\n\n'
    )


def test_partial_failure_note_wire_fixture():
    ev = Event(
        kind="note",
        payload=NoteEvent(
            text="Run diverged from the plan",
            kind="partial_failure",
            failed_steps=["step_3", "step_5"],
        ).model_dump(),
    )
    assert _sse_encode(ev) == (
        b"event: note\n"
        b'data: {"text":"Run diverged from the plan","provider":null,'
        b'"kind":"partial_failure","last_step_id":null,"run_id":null,'
        b'"failed_steps":["step_3","step_5"]}\n\n'
    )


# --- test-review round 1 regressions ---------------------------------------


@pytest.mark.asyncio
async def test_no_banner_when_recovered_on_last_allowed_retry(tracer):
    """Round 2, NEW-1 regression: consuming the FULL allowance and then
    recovering on the last allowed attempt is not a terminal failure. The
    over-broad 'allowance fully consumed' check flagged this green run; the
    step must carry no partial-failure note."""
    steps = _tool_steps("tool_1")
    state = _state(steps, {})
    # transient: two failures (both retried) then success — retry_count 0/1/2.
    tools = _ScriptedTools(
        [
            ToolUnavailableError("MCP 503: overloaded"),
            ToolUnavailableError("MCP 503: overloaded"),
            {"items": []},
        ]
    )

    await executor_module.executor_node(
        state,
        settings=_settings(),
        tracer=tracer,
        budget=_StubBudget(),
        rag=None,  # type: ignore[arg-type]
        tools=tools,
    )

    assert [e for e in state["events_so_far"] if e.kind == "note"] == []
    final = [e for e in state["events_so_far"] if e.kind == "tool_result"][-1]
    assert final.payload["ok"] is True
    assert final.payload["retry_count"] == 2  # full allowance consumed…
    assert state.get("tool_terminal_failures") == {}  # …but recovered
    step_ends = [e for e in state["events_so_far"] if e.kind == "step_end"]
    assert [e.payload["status"] for e in step_ends] == ["ok"]


@pytest.mark.asyncio
async def test_second_retry_reprompt_carries_prior_exchange(tracer, monkeypatch):
    """Round 2, NEW-3 regression: the retry-2 re-prompt must carry the first
    rejection's exchange so every assistant tool-call turn in the message
    list is answered — without the carry, `assistant(tc_1)` sat unanswered
    followed by `assistant(tc_2)`, a shape strict providers 400 on."""
    step = PlanStep(id="react_1", kind="react", prompt="p", max_iterations=2)
    state = _state([step], {})
    tools = _ScriptedTools(
        [
            ToolError("list_feeds: bad args"),
            ToolError("list_feeds: still bad"),
            {"items": []},  # third attempt (second retry) succeeds
        ]
    )
    _use_llm(
        monkeypatch,
        [
            _tool_call(args={"limit": 1}, tc_id="tc_1"),
            _tool_call(args={"limit": 2}, tc_id="tc_2"),
            _tool_call(args={"limit": 3}, tc_id="tc_3"),
            AIMessage(content="done"),
        ],
    )

    with tracer.start_as_current_span("x") as span:
        await _run_react(state, step, _settings(), tracer, _StubBudget(), tools, span)

    # captured[0]=iter0 live call, [1]=retry-1 re-prompt, [2]=retry-2 re-prompt.
    reprompt2 = _ScriptedLlm.captured[2]
    assistant_ids = [
        tc["id"]
        for m in reprompt2
        if isinstance(m, AIMessage)
        for tc in (getattr(m, "tool_calls", None) or [])
    ]
    tool_ids = [m.tool_call_id for m in reprompt2 if isinstance(m, ToolMessage)]
    # Every assistant call (the live tc_1 + the carried corrected tc_2) is
    # answered exactly once — no unanswered turns, no duplicates.
    assert assistant_ids == ["tc_1", "tc_2"]
    assert tool_ids == ["tc_1", "tc_2"]


@pytest.mark.asyncio
async def test_parallel_tool_calls_placeholders_answer_siblings(tracer, monkeypatch):
    """Review round 3: a re-prompt for ONE call of a parallel batch must not
    leave its siblings unanswered — strict OpenAI-compatible providers 400
    on a tool call without a matching response message."""
    step = PlanStep(id="react_1", kind="react", prompt="p", max_iterations=2)
    state = _state([step], {})
    # tc_a is rejected once then corrected; tc_b never fails.
    tools = _ScriptedTools([ToolError("list_feeds: bad args"), {"items": []}])
    parallel_turn = AIMessage(
        content="",
        tool_calls=[
            {"id": "tc_a", "name": "list_feeds", "args": {"limit": "x"}},
            {"id": "tc_b", "name": "get_feed", "args": {"id": 1}},
        ],
    )
    _use_llm(
        monkeypatch,
        [
            parallel_turn,
            _tool_call(name="list_feeds", args={"limit": 5}, tc_id="tc_a2"),
            AIMessage(content="done"),
        ],
    )

    with tracer.start_as_current_span("x") as span:
        await _run_react(state, step, _settings(), tracer, _StubBudget(), tools, span)

    reprompt = _ScriptedLlm.captured[1]  # the re-prompt for tc_a
    assistant_ids = [
        tc["id"]
        for m in reprompt
        if isinstance(m, AIMessage)
        for tc in (getattr(m, "tool_calls", None) or [])
    ]
    tool_ids = [m.tool_call_id for m in reprompt if isinstance(m, ToolMessage)]
    # Both calls of the parallel turn are answered — the rejected one by the
    # real rejection, the sibling by a placeholder.
    assert set(assistant_ids) == {"tc_a", "tc_b"}
    assert set(tool_ids) == {"tc_a", "tc_b"}
    assert [i for i in assistant_ids if i not in tool_ids] == []


@pytest.mark.asyncio
async def test_react_reprompt_does_not_duplicate_assistant_call(tracer, monkeypatch):
    """F2 regression (test-review round 1): in the ReAct path the live
    messages already end with the assistant turn that emitted the bad call —
    the re-prompt must NOT append a second synthetic copy (two consecutive
    assistant tool-call turns; strict providers reject that shape)."""
    step = PlanStep(id="react_1", kind="react", prompt="p", max_iterations=2)
    state = _state([step], {})
    tools = _ScriptedTools([ToolError("list_feeds: bad args"), {"items": []}])
    # Same call id on every model turn, mirroring the live loop.
    tc = _tool_call(args={"limit": 1})
    _use_llm(monkeypatch, [tc, tc, AIMessage(content="done")])

    with tracer.start_as_current_span("x") as span:
        await _run_react(state, step, _settings(), tracer, _StubBudget(), tools, span)

    reprompt = _ScriptedLlm.captured[1]  # captured[0] is iteration 0's live call
    assistant_turns = [
        m for m in reprompt if isinstance(m, AIMessage) and getattr(m, "tool_calls", None)
    ]
    assert len(assistant_turns) == 1, "assistant tool-call turn must appear exactly once"
    assert assistant_turns[0].tool_calls[0]["id"] == "tc_1"
    tool_msgs = [m for m in reprompt if isinstance(m, ToolMessage)]
    assert tool_msgs[-1].tool_call_id == "tc_1"  # rejection answers the same call


def test_compute_backoff_ms_bounds_and_ceiling():
    """Spec: min(base · 2^attempt ± 20% jitter, ceiling). Jitter bounds asserted
    loosely (the exact draw is random); the ceiling clamp asserted exactly."""
    settings = Settings(
        llm_base_url="http://stub",
        metrics_port=0,
        tool_retry_backoff_base_ms=250,
        tool_retry_backoff_ceiling_ms=5000,
    )
    assert 200 <= _compute_backoff_ms(0, settings) <= 300  # 250 ± 20%
    assert 400 <= _compute_backoff_ms(1, settings) <= 600  # 500 ± 20%
    assert _compute_backoff_ms(10, settings) == 5000.0  # 256000 → clamped


@pytest.mark.asyncio
async def test_tool_retry_max_zero_fails_fast(tracer):
    """Spec / N4: `tool_retry_max=0` → exactly one attempt, no counter
    increment, no model re-prompt, terminal frame with retry_count=0."""
    settings = Settings(
        llm_base_url="http://stub",
        metrics_port=0,
        tool_retry_max=0,
        tool_retry_backoff_base_ms=0,
    )
    steps = _tool_steps("tool_1")
    state = _state(steps, {})
    tools = _ScriptedTools([ToolError("list_feeds: bad args")])

    before = _retry_counter("list_feeds", "tool_arg_invalid")
    with tracer.start_as_current_span("x") as span, pytest.raises(RuntimeError):
        await _run_tool(
            state,
            steps[0],
            settings=settings,
            tracer=tracer,
            budget=_StubBudget(),
            tools=tools,
            span=span,
        )

    assert len(tools.calls) == 1
    assert _retry_counter("list_feeds", "tool_arg_invalid") - before == 0
    assert _CapturingLlm.captured == []  # the autouse stub saw no re-prompt
    final = [e for e in state["events_so_far"] if e.kind == "tool_result"][-1]
    assert final.payload["ok"] is False
    assert final.payload["retry_count"] == 0


@pytest.mark.asyncio
async def test_first_try_success_does_not_increment_retry_counters(tracer):
    """Spec: a successful first attempt increments neither reason counter and
    carries retry_count=0."""
    steps = _tool_steps("tool_1")
    state = _state(steps, {})
    tools = _ScriptedTools([{"items": []}])

    before_arg = _retry_counter("list_feeds", "tool_arg_invalid")
    before_transient = _retry_counter("list_feeds", "tool_execution_transient")
    with tracer.start_as_current_span("x") as span:
        await _run_tool(
            state,
            steps[0],
            settings=_settings(),
            tracer=tracer,
            budget=_StubBudget(),
            tools=tools,
            span=span,
        )

    assert _retry_counter("list_feeds", "tool_arg_invalid") - before_arg == 0
    assert _retry_counter("list_feeds", "tool_execution_transient") - before_transient == 0
    final = [e for e in state["events_so_far"] if e.kind == "tool_result"][-1]
    assert final.payload["ok"] is True
    assert final.payload["retry_count"] == 0


@pytest.mark.asyncio
async def test_retry_sets_span_attributes(monkeypatch):
    """Spec: the surrounding `agent.step.tool` span carries `retry_count` and
    `retry_reason` so operators can correlate retries with the run trace."""
    exporter = InMemorySpanExporter()
    provider = _SdkTracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    sdk_tracer = provider.get_tracer("test-executor-span-attrs")

    steps = _tool_steps("tool_1")
    state = _state(steps, {})
    tools = _ScriptedTools([ToolError("list_feeds: bad args"), {"items": []}])
    _use_llm(monkeypatch, [_tool_call(args={"limit": 5})])

    try:
        with sdk_tracer.start_as_current_span("agent.step.tool") as span:
            await _run_tool(
                state,
                steps[0],
                settings=_settings(),
                tracer=sdk_tracer,
                budget=_StubBudget(),
                tools=tools,
                span=span,
            )
        spans = exporter.get_finished_spans()
        assert spans, "expected one finished step span"
        attrs = spans[0].attributes
        assert attrs["retry_count"] == 1
        assert attrs["retry_reason"] == "tool_arg_invalid"
    finally:
        provider.shutdown()


@pytest.mark.asyncio
async def test_banner_fires_when_retry_budget_split_across_dispatches(
    tracer, monkeypatch
):
    """F3 regression (test-review round 1): a budget exhausted by TWO
    dispatches (1 retry each) — ReAct iteration 1 succeeds after one retry
    (used=1), iteration 2 exhausts the remaining one (terminal frame
    retry_count=1 ≠ retry_max) — must still flag the step. The literal
    `retry_count == tool_retry_max` clause alone misses it."""
    step = PlanStep(id="react_1", kind="react", prompt="p", max_iterations=3)
    state = _state([step], {})
    tools = _ScriptedTools(
        [
            ToolError("list_feeds: bad args"),
            {"items": []},  # iteration-1 dispatch recovers after 1 retry
            ToolError("list_feeds: bad args"),
            ToolError("list_feeds: bad args"),  # iteration-2 dispatch exhausts
        ]
    )
    tc = _tool_call(args={"limit": 1})
    _use_llm(monkeypatch, [tc, tc, tc, tc, AIMessage(content="done")])

    await executor_module.executor_node(
        state,
        settings=_settings(),
        tracer=tracer,
        budget=_StubBudget(),
        rag=None,  # type: ignore[arg-type]
        tools=tools,
    )

    notes = [e for e in state["events_so_far"] if e.kind == "note"]
    assert len(notes) == 1
    assert notes[0].payload["failed_steps"] == ["react_1"]
    # Prove the split: no failure frame ever carried the full retry_count.
    failures = [
        e
        for e in state["events_so_far"]
        if e.kind == "tool_result" and e.payload["ok"] is False
    ]
    assert failures, "expected failure frames"
    assert all(f.payload["retry_count"] < 2 for f in failures)


# --- cross-runtime wire fixtures: agent-native web tools (agent-web-tools 7.1)
# Full-stream fixture replayed by frontend/tests/e2e/chat-web-tools.spec.ts.
# The producer test REGENERATES the stream from the real executor node and
# demands byte equality — any frame-shape drift fails here and, through the
# shared bytes, in the Playwright spec.

import itertools
import pathlib

WEB_TOOLS_FIXTURE = (
    pathlib.Path(__file__).resolve().parents[3]
    / "frontend"
    / "tests"
    / "e2e"
    / "fixtures"
    / "web-tools-search-fetch.sse"
)

FIXTURE_RUN_ID = "00000000-0000-4000-8000-000000000007"


class _FixtureLlm:
    """Deterministic answering LLM for the fixture stream."""

    def __init__(self, *args, **kwargs):
        pass

    async def ainvoke(self, messages):
        return AIMessage(
            content="The Nyx agent decomposes a request into ordered steps and can search the web."
        )


class _WebScriptedTools:
    def __init__(self, responses):
        self.responses = list(responses)

    async def list_tools(self):
        return []

    async def call(self, name, arguments):
        return self.responses.pop(0)


def _web_tools_plan() -> Plan:
    return Plan(
        steps=[
            PlanStep(
                id="step_1",
                kind="tool",
                tool="web_search",
                args={"query": "nyx agent"},
            ),
            PlanStep(
                id="step_2",
                kind="tool",
                tool="web_fetch",
                args={"url": "https://docs.nyx.test/agent"},
            ),
            PlanStep(
                id="step_3",
                kind="llm",
                prompt="Answer using the gathered web data.",
            ),
        ]
    )


def _web_tool_results() -> list[dict]:
    """Results shaped exactly like agent.web_tools returns them (via the
    real framer, so a framing change regenerates the fixture too)."""
    from agent.web_tools import frame_untrusted

    search_url = "https://docs.nyx.test/agent"
    return [
        {
            "query": "nyx agent",
            "results": [
                {
                    "title": frame_untrusted("Nyx agent docs", search_url),
                    "url": search_url,
                    "description": frame_untrusted(
                        "Plan-and-execute orchestrator.", search_url
                    ),
                }
            ],
        },
        {
            "url": search_url,
            "status": 200,
            "content_type": "text/html",
            "text": frame_untrusted(
                "The Nyx agent runs a planner, then an executor.", search_url
            ),
        },
    ]


async def _build_web_tools_stream(tracer) -> bytes:
    """Regenerate the fixture's full SSE stream via the real producer.

    uuid4 and ChatOpenAI are pinned for the duration so the stream is
    deterministic; both are restored on exit.
    """
    counter = itertools.count(1)
    original_uuid4 = executor_module.uuid.uuid4
    original_chat = executor_module.ChatOpenAI
    executor_module.uuid.uuid4 = lambda: f"00000000-0000-4000-8000-{next(counter):012d}"
    executor_module.ChatOpenAI = _FixtureLlm
    try:
        plan = _web_tools_plan()
        state = _state(plan.steps, {})
        await executor_module.executor_node(
            state,
            settings=_settings(),
            tracer=tracer,
            budget=_StubBudget(),
            rag=None,  # type: ignore[arg-type]
            tools=_WebScriptedTools(_web_tool_results()),
        )
    finally:
        executor_module.uuid.uuid4 = original_uuid4
        executor_module.ChatOpenAI = original_chat

    frames = [
        _sse_encode(
            Event(kind="run_id", payload=RunIDEvent(run_id=FIXTURE_RUN_ID).model_dump())
        ),
        _sse_encode(
            Event(
                kind="note",
                payload=NoteEvent(
                    text="fresh_run", kind="fresh_run", run_id=FIXTURE_RUN_ID
                ).model_dump(),
            )
        ),
        _sse_encode(Event(kind="plan", payload=PlanEvent(steps=plan.steps).model_dump())),
        *(_sse_encode(ev) for ev in state["events_so_far"]),
        _sse_encode(Event(kind="done", payload=DoneEvent().model_dump())),
        _sse_encode(
            Event(kind="terminator", payload=TerminatorEvent(reason=None).model_dump())
        ),
    ]
    return b"".join(frames) + b"data: [DONE]\n\n"


@pytest.mark.asyncio
async def test_web_tools_wire_fixture_matches_producer(tracer):
    """The committed .sse fixture must equal what the producer emits today.

    To regenerate after an intentional frame change (and then re-run the
    Playwright spec): run this helper from backend/agent_py with the venv
    python and write its output to WEB_TOOLS_FIXTURE.
    """
    generated = await _build_web_tools_stream(tracer)
    assert WEB_TOOLS_FIXTURE.exists(), f"missing fixture: {WEB_TOOLS_FIXTURE}"
    assert generated == WEB_TOOLS_FIXTURE.read_bytes(), (
        "SSE wire drift: regenerate the fixture and re-run chat-web-tools.spec.ts"
    )


# --- context-overflow guard (2026-10-14) ------------------------------------
# Motivated by an in-the-wild repro: a `web_search` step returning 5 results
# inflated the answer step's prompt past the 4B small-model context window;
# the provider rejected with OpenAIInvalidRequestError 400. The fix is
# in `_render_prior_data`: bound the per-section and total block sizes.
# These tests pin both caps and the "keep most recent, drop older" policy.


def test_render_prior_data_caps_per_section():
    from agent.executor import (
        PRIOR_DATA_PER_SECTION_MAX_CHARS,
        _render_prior_data,
    )

    big = "x" * (PRIOR_DATA_PER_SECTION_MAX_CHARS * 2)
    state = _state(
        [
            PlanStep(id="data_1", kind="tool", tool="web_search"),
            PlanStep(id="answer", kind="llm", prompt="reply"),
        ],
        {"data_1": {"results": [{"title": big, "url": "u", "description": "d"}]}},
    )

    block = _render_prior_data(state, "answer")
    assert len(block) < len(big) + 200
    assert "truncated: section exceeded" in block


def test_render_prior_data_caps_total_and_keeps_most_recent():
    from agent.executor import (
        PRIOR_DATA_MAX_CHARS,
        PRIOR_DATA_PER_SECTION_MAX_CHARS,
        _render_prior_data,
    )

    # Three prior steps, each at the per-section cap; their sum
    # (3 × PRIOR_DATA_PER_SECTION_MAX_CHARS = 18000) exceeds the total
    # cap (PRIOR_DATA_MAX_CHARS = 12000), so older sections must drop.
    payload_size = PRIOR_DATA_PER_SECTION_MAX_CHARS
    state = _state(
        [
            PlanStep(id="data_old", kind="tool", tool="web_search"),
            PlanStep(id="data_mid", kind="tool", tool="web_search"),
            PlanStep(id="data_new", kind="tool", tool="web_search"),
            PlanStep(id="answer", kind="llm", prompt="reply"),
        ],
        {
            "data_old": {"results": [{"title": "o" * payload_size}]},
            "data_mid": {"results": [{"title": "m" * payload_size}]},
            "data_new": {"results": [{"title": "n" * payload_size}]},
        },
    )

    block = _render_prior_data(state, "answer")
    # The most recent section must survive. The total cap allows ONE
    # full per-section-cap section (6000 chars) plus a smaller partial
    # second; the policy drops older sections first.
    assert "data_new" in block
    # The oldest section must not survive when the cap requires dropping.
    assert "data_old" not in block
    # Total block stays under the cap (with small slack for the header
    # + any per-section truncation marker on the kept second section).
    assert len(block) <= PRIOR_DATA_MAX_CHARS + 200


def test_render_prior_data_single_huge_section_caps_to_total_budget():
    """If a single section alone would blow the total cap (after the
    per-section cap already trimmed it), it must still get a final cap
    instead of starving the answer step entirely."""
    from agent.executor import PRIOR_DATA_MAX_CHARS, _render_prior_data

    state = _state(
        [
            PlanStep(id="data_1", kind="tool", tool="web_search"),
            PlanStep(id="answer", kind="llm", prompt="reply"),
        ],
        {
            "data_1": {
                "results": [
                    {"title": "a", "url": "u", "description": "x" * 100000}
                ]
            }
        },
    )

    block = _render_prior_data(state, "answer")
    # The final block must be at most the total cap, with a marker.
    assert len(block) <= PRIOR_DATA_MAX_CHARS + 200
    assert "truncated" in block


def test_render_prior_data_strips_untrusted_markers():
    """The wire-side untrusted-content markers must not be re-pasted
    into the answer step's prompt — they double the per-result
    character count and exhaust small-model context windows (the
    2026-10-14 live Qwen3-4B 400 incident). The text content itself
    is preserved.
    """
    from agent.executor import _render_prior_data

    framed = (
        '<untrusted_web_content source="https://x.test/agent">\n'
        "正文 — important content.\n"
        "</untrusted_web_content>"
    )
    state = _state(
        [
            PlanStep(id="data_1", kind="tool", tool="web_search"),
            PlanStep(id="answer", kind="llm", prompt="reply"),
        ],
        {"data_1": {"results": [{"title": "t", "url": "u", "description": framed}]}},
    )

    block = _render_prior_data(state, "answer")
    assert "<untrusted_web_content" not in block
    assert "</untrusted_web_content>" not in block
    # The section JSON is dumped with ensure_ascii (default), so Chinese
    # renders as \uXXXX escapes — decode the section to check the
    # content value survived the strip.
    import json as _json

    section = block.split("[data_1 — tool result from web_search]\n", 1)[1]
    desc = _json.loads(section)["results"][0]["description"]
    assert "正文 — important content" in desc


# --- user-prefs injection (openspec/changes/agent-chat-fixes 1.1/1.2) -----
#
# The executor writes the user-visible answer: both answer paths (`llm`
# and `react`) must carry the rendered prefs block, and must stay
# byte-identical to their pre-injection prompt shape when the block is
# empty or absent. The planner-side equivalent lives in
# `tests/test_prefs_injection.py`; the app-level wire check (recall
# question + kill switch) lives there too.


def _prefs_block() -> str:
    from agent.prefs import Pref, prefs_block

    return prefs_block([Pref(key="snack", value="鱿鱼丝")])


def _answer_step(kind: str) -> PlanStep:
    return PlanStep(id="s_1", kind=kind, prompt="Answer.", max_iterations=1)


async def _run_answer_step(kind, step, state, tracer, **kwargs):
    with tracer.start_as_current_span("x") as span:
        if kind == "llm":
            await _run_llm(
                state, step, _settings(), tracer, _StubBudget(), span, **kwargs
            )
        else:
            await _run_react(
                state,
                step,
                _settings(),
                tracer,
                _StubBudget(),
                _StubTools(),
                span,
                **kwargs,
            )


@pytest.mark.asyncio
@pytest.mark.parametrize("kind", ["llm", "react"])
async def test_answer_prompt_appends_the_prefs_block(kind, tracer):
    """Spec "Preferences reach the executor that writes the reply": the
    block is appended to the step's system prompt on BOTH answer paths."""
    step = _answer_step(kind)
    state = _state([step], {})
    block = _prefs_block()

    await _run_answer_step(kind, step, state, tracer, prefs_text=block)

    system = _CapturingLlm.captured[0][0]
    assert system.type == "system"
    assert system.content == "Answer." + block
    assert "- snack" in system.content
    assert "asks what they like" in system.content, (
        "the lead-in covers recall questions on the answer path too"
    )


@pytest.mark.asyncio
@pytest.mark.parametrize("kind", ["llm", "react"])
async def test_answer_prompt_is_byte_identical_without_prefs(kind, tracer):
    """Spec "Empty prefs leave the prompt untouched": the pre-injection
    prompt shape is preserved exactly, whether the caller omits
    `prefs_text` entirely or passes the empty rendered block."""
    step = _answer_step(kind)

    await _run_answer_step(kind, step, _state([step], {}), tracer)
    assert _CapturingLlm.captured[0][0].content == "Answer."

    _CapturingLlm.captured = []
    await _run_answer_step(kind, step, _state([step], {}), tracer, prefs_text="")
    assert _CapturingLlm.captured[0][0].content == "Answer."


@pytest.mark.asyncio
@pytest.mark.parametrize("kind", ["llm", "react"])
async def test_executor_node_threads_prefs_from_the_factory(kind, tracer):
    """D1: the block rides the graph's node closure into `executor_node`
    — and the factory's `""` default keeps pre-existing callers'
    prompts byte-identical."""
    from agent.graph import _executor_step_factory

    step = _answer_step(kind)
    block = _prefs_block()
    node = _executor_step_factory(
        _settings(),
        _StubBudget(),
        None,
        _StubTools(),
        tracer,
        prefs_text=block,
    )

    await node(_state([step], {}))
    assert _CapturingLlm.captured[0][0].content == "Answer." + block

    _CapturingLlm.captured = []
    default_node = _executor_step_factory(
        _settings(), _StubBudget(), None, _StubTools(), tracer
    )
    await default_node(_state([step], {}))
    assert _CapturingLlm.captured[0][0].content == "Answer."
