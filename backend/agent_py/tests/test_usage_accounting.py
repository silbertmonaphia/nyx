"""Real-token usage accounting (done-frame usage reflects actual LLM spend).

Covers the accumulator helpers on `AgentState`, the executor call sites
(llm step, ReAct iteration, tool-arg re-prompt), the planner's structured
+ legacy paths (usage captured through the real
`UsageMetadataCallbackHandler` consumer), and the terminal `done` frame
emission in `graph.py`.
"""

from __future__ import annotations

import asyncio
import json

import pytest
from langchain_core.language_models.fake_chat_models import GenericFakeChatModel
from langchain_core.messages import AIMessage
from prometheus_client import REGISTRY

from agent.config import Settings
from agent.executor import (
    _reprompt_for_corrected_call,
    _run_llm,
    _run_react,
    _terminate_with_error,
)
from agent.graph import run_agent_stream
from agent.planner import _emit_error, _emit_legacy_plan, _emit_structured_plan
from agent.rag_client import RagClient
from agent.state import (
    accumulate_usage,
    accumulate_usage_metadata,
    new_state,
    usage_from_message,
)
from agent.types import Event, Plan, PlanStep

MODEL = "stub-model"


class _NoopTracer:
    def start_as_current_span(self, name):
        from contextlib import nullcontext

        span = type("Span", (), {"set_attribute": lambda self, k, v: None})()
        return nullcontext(span)


class _StubBudget:
    class _D:
        decision = "ok"

    async def check_and_increment(self, user_id, tokens, tracer):
        return self._D()


class _StubTools:
    def __init__(self, script=None, schemas=None):
        self.script = list(script or [])
        self.schemas = schemas or []

    async def list_tools(self):
        return self.schemas

    async def call(self, name, arguments):
        outcome = self.script.pop(0) if len(self.script) > 1 else self.script[0]
        if isinstance(outcome, Exception):
            raise outcome
        return outcome


def _settings() -> Settings:
    return Settings(
        _env_file=None,
        llm_base_url="http://stub",
        llm_model=MODEL,
        metrics_port=0,
    )


def _planner_settings(monkeypatch, structured: str = "true") -> Settings:
    monkeypatch.setenv("PYTHON_AGENT_LLM_BASE_URL", "http://planner.test/v1")
    monkeypatch.setenv("PYTHON_AGENT_LLM_API_KEY", "test-key")
    monkeypatch.setenv("PYTHON_AGENT_LLM_MODEL", MODEL)
    monkeypatch.setenv("PYTHON_AGENT_PLANNER_RETRY_MAX", "1")
    monkeypatch.setenv("PYTHON_AGENT_PLANNER_STRUCTURED_ENABLED", structured)
    monkeypatch.setenv("PYTHON_AGENT_PLAN_MAX_STEPS", "5")
    return Settings(_env_file=None)


def _state():
    return new_state(
        user_id=1,
        run_id="usage-run",
        messages=[{"role": "user", "content": "hi"}],
        model=MODEL,
    )


def _ai(content: str, prompt: int, completion: int, tool_calls=None) -> AIMessage:
    return AIMessage(
        content=content,
        tool_calls=tool_calls or [],
        usage_metadata={
            "input_tokens": prompt,
            "output_tokens": completion,
            "total_tokens": prompt + completion,
        },
        response_metadata={"model_name": MODEL},
    )


def _await(coro):
    return asyncio.run(coro)


def _tokens(role: str, model: str) -> float:
    return (
        REGISTRY.get_sample_value("agent_tokens_total", {"role": role, "model": model})
        or 0.0
    )


def _plan_json() -> str:
    return json.dumps({"steps": [{"id": "s1", "kind": "llm", "prompt": "p"}]})




def test_usage_from_message_prefers_usage_metadata():
    msg = AIMessage(
        content="x",
        usage_metadata={"input_tokens": 12, "output_tokens": 3, "total_tokens": 15},
        response_metadata={
            "token_usage": {
                "prompt_tokens": 99,
                "completion_tokens": 99,
                "total_tokens": 198,
            }
        },
    )
    assert usage_from_message(msg) == (12, 3, 15)


def test_usage_from_message_falls_back_to_wire_token_usage():
    msg = AIMessage(
        content="x",
        response_metadata={
            "token_usage": {
                "prompt_tokens": 7,
                "completion_tokens": 2,
                "total_tokens": 9,
            }
        },
    )
    assert usage_from_message(msg) == (7, 2, 9)


def test_usage_from_message_without_usage_yields_zeros():
    assert usage_from_message(AIMessage(content="x")) == (0, 0, 0)


def test_accumulate_usage_sums_across_calls():
    state = _state()
    assert accumulate_usage(state, _ai("a", 10, 5)) == (10, 5, 15)
    assert accumulate_usage(state, _ai("b", 7, 2)) == (7, 2, 9)
    assert state["cumulative_prompt_tokens"] == 17
    assert state["cumulative_completion_tokens"] == 7
    assert state["cumulative_tokens"] == 24


def test_accumulate_usage_tolerates_missing_state_keys():
    state = _state()
    for key in (
        "cumulative_tokens",
        "cumulative_prompt_tokens",
        "cumulative_completion_tokens",
    ):
        del state[key]
    accumulate_usage(state, _ai("a", 4, 1))
    assert state["cumulative_tokens"] == 5


def test_accumulate_usage_metadata_sums_raw_dicts():
    state = _state()
    accumulate_usage_metadata(state, {"input_tokens": 3, "output_tokens": 4})
    accumulate_usage_metadata(state, None)
    assert state["cumulative_tokens"] == 7
    assert state["cumulative_prompt_tokens"] == 3




@pytest.mark.asyncio
async def test_llm_step_accrues_real_usage(tracer, monkeypatch):
    from agent import executor as executor_module

    class _UsageLlm:
        def __init__(self, *args, **kwargs):
            pass

        async def ainvoke(self, messages):
            return _ai("answer", 120, 34)

    monkeypatch.setattr(executor_module, "ChatOpenAI", _UsageLlm)
    step = PlanStep(id="llm_1", kind="llm", prompt="p")
    state = _state()
    before = _tokens("executor", MODEL)
    with tracer.start_as_current_span("x") as span:
        await _run_llm(state, step, _settings(), tracer, _StubBudget(), span)
    assert state["cumulative_prompt_tokens"] == 120
    assert state["cumulative_completion_tokens"] == 34
    assert state["cumulative_tokens"] == 154
    assert _tokens("executor", MODEL) - before == 154


@pytest.mark.asyncio
async def test_react_loop_accrues_usage_per_iteration(tracer, monkeypatch):
    from agent import executor as executor_module

    responses = [
        _ai("", 50, 10, tool_calls=[{"id": "tc_1", "name": "any_tool", "args": {}}]),
        _ai("final answer", 60, 20),
    ]

    class _UsageReActLlm:
        def __init__(self, *args, **kwargs):
            pass

        def bind_tools(self, schemas):
            return self

        async def ainvoke(self, messages):
            return responses.pop(0)

    monkeypatch.setattr(executor_module, "ChatOpenAI", _UsageReActLlm)
    step = PlanStep(id="react_1", kind="react", prompt="p", max_iterations=3)
    state = _state()
    with tracer.start_as_current_span("x") as span:
        await _run_react(
            state,
            step,
            _settings(),
            tracer,
            _StubBudget(),
            _StubTools(script=[{"result": 1}]),
            span,
        )
    assert state["cumulative_prompt_tokens"] == 110
    assert state["cumulative_completion_tokens"] == 30
    assert state["cumulative_tokens"] == 140


@pytest.mark.asyncio
async def test_reprompt_accrues_usage(tracer, monkeypatch):
    from agent import executor as executor_module

    class _UsageRepromptLlm:
        def __init__(self, *args, **kwargs):
            pass

        def bind_tools(self, schemas):
            return self

        async def ainvoke(self, messages):
            return _ai(
                "",
                80,
                9,
                tool_calls=[{"id": "tc_fix", "name": "list_feeds", "args": {"limit": 5}}],
            )

    monkeypatch.setattr(executor_module, "ChatOpenAI", _UsageRepromptLlm)
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
    tools = _StubTools(script=[{"result": 1}], schemas=schemas)
    state = _state()
    with tracer.start_as_current_span("x"):
        result = await _reprompt_for_corrected_call(
            state,
            step_prompt="p",
            tool_name="list_feeds",
            args={"limit": "x"},
            error_text="limit must be an integer",
            base_messages=None,
            model_tool_call_id="tc_bad",
            carry=[],
            settings=_settings(),
            tracer=tracer,
            budget=_StubBudget(),
            tools=tools,
        )
    assert result is not None
    assert state["cumulative_prompt_tokens"] == 80
    assert state["cumulative_completion_tokens"] == 9
    assert state["cumulative_tokens"] == 89




def _patch_structured_chat(monkeypatch, messages: list[AIMessage]) -> None:
    """Route the REAL structured chain over a fake chat model carrying usage."""

    class _UsageChat(GenericFakeChatModel):
        def __init__(self, **_kwargs):
            super().__init__(messages=iter(messages))

    monkeypatch.setattr("agent.planner.ChatOpenAI", _UsageChat)


def test_structured_planner_accrues_usage_first_try(monkeypatch):
    settings = _planner_settings(monkeypatch)
    _patch_structured_chat(monkeypatch, [_ai(_plan_json(), 210, 25)])
    state = _state()
    before = _tokens("planner", MODEL)
    plan_or_error = _await(_emit_structured_plan(state, settings, _NoopTracer()))
    assert isinstance(plan_or_error, Plan)
    assert state["cumulative_prompt_tokens"] == 210
    assert state["cumulative_completion_tokens"] == 25
    assert state["cumulative_tokens"] == 235
    assert _tokens("planner", MODEL) - before == 235


def test_structured_planner_accrues_usage_across_retries(monkeypatch):
    settings = _planner_settings(monkeypatch)
    _patch_structured_chat(
        monkeypatch, [_ai("not json", 180, 12), _ai(_plan_json(), 220, 18)]
    )
    state = _state()
    plan_or_error = _await(_emit_structured_plan(state, settings, _NoopTracer()))
    assert isinstance(plan_or_error, Plan)
    assert state["cumulative_prompt_tokens"] == 400
    assert state["cumulative_completion_tokens"] == 30
    assert state["cumulative_tokens"] == 430


def test_legacy_planner_accrues_usage(monkeypatch):
    settings = _planner_settings(monkeypatch, structured="false")

    class _UsageLegacyClient:
        def __init__(self, *args, **kwargs):
            pass

        async def ainvoke(self, messages):
            return _ai(_plan_json(), 90, 11)

    monkeypatch.setattr("agent.planner.ChatOpenAI", _UsageLegacyClient)
    state = _state()
    before = _tokens("planner", MODEL)
    plan_or_error = _await(_emit_legacy_plan(state, settings, _NoopTracer()))
    assert isinstance(plan_or_error, Plan)
    assert state["cumulative_prompt_tokens"] == 90
    assert state["cumulative_completion_tokens"] == 11
    assert state["cumulative_tokens"] == 101
    assert _tokens("planner", MODEL) - before == 101


def test_planner_error_done_frame_carries_real_usage():
    state = _state()
    state["cumulative_prompt_tokens"] = 33
    state["cumulative_completion_tokens"] = 6
    state["cumulative_tokens"] = 39
    _emit_error(state, "plan_invalid", "Plan is invalid", reason="failed")
    done = [e for e in state["events_so_far"] if e.kind == "done"][-1]
    assert done.payload["usage"] == {
        "prompt_tokens": 33,
        "completion_tokens": 6,
        "total_tokens": 39,
    }


def test_executor_termination_done_frame_carries_real_usage():
    state = _state()
    state["cumulative_prompt_tokens"] = 120
    state["cumulative_completion_tokens"] = 30
    state["cumulative_tokens"] = 150
    _terminate_with_error(state, "budget_exceeded", "Budget exceeded", _settings())
    done = [e for e in state["events_so_far"] if e.kind == "done"][-1]
    assert done.payload["usage"] == {
        "prompt_tokens": 120,
        "completion_tokens": 30,
        "total_tokens": 150,
    }




@pytest.fixture
def tracer():
    from opentelemetry import trace

    return trace.get_tracer("test-usage-accounting")


@pytest.mark.asyncio
async def test_done_frame_carries_real_usage(tracer, monkeypatch):
    async def fake_plan_node(state, *args, **kwargs):
        state["plan"] = Plan(steps=[PlanStep(id="s1", kind="llm", prompt="p")])
        state["cumulative_prompt_tokens"] = 120
        state["cumulative_completion_tokens"] = 30
        state["cumulative_tokens"] = 150
        return state

    async def fake_executor_node(state, *args, **kwargs):
        state["events_so_far"].append(Event(kind="delta", payload={"delta": "answer"}))
        return state

    monkeypatch.setattr("agent.graph.plan_node", fake_plan_node)
    monkeypatch.setattr("agent.graph.executor_node", fake_executor_node)

    state = _state()
    events = [
        ev
        async for ev in run_agent_stream(
            state,
            settings=_settings(),
            tracer=tracer,
            budget=_StubBudget(),
            rag=RagClient("http://stub", "t"),
            tools=_StubTools(),
        )
    ]
    done = [e for e in events if e.kind == "done"]
    assert done and done[0].payload["usage"] == {
        "prompt_tokens": 120,
        "completion_tokens": 30,
        "total_tokens": 150,
    }
