"""Executor unit tests — data-step outputs threaded into answering steps.

`_render_prior_data` is the mechanism that lets a plan like [rag, llm]
actually see the retrieved feeds. Motivating bug (2026-10): "summarize
recent 3 months feed" planned a rag-only plan — and even with a
[rag, llm] plan the llm step's messages never included the retrieved
context_block because `step_outputs` was write-only.
"""

from __future__ import annotations

import pytest
from langchain_core.messages import AIMessage
from opentelemetry import trace

from agent import executor as executor_module
from agent.config import Settings
from agent.executor import _run_llm, _run_react
from agent.state import new_state
from agent.types import ChatMessage, Plan, PlanStep


class _CapturingLlm:
    """Drop-in ChatOpenAI stub that records the message lists it receives."""

    captured: list = []

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
    return Settings(llm_base_url="http://stub", metrics_port=0)


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
