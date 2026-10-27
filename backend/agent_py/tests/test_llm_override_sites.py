"""Credential threading at every `ChatOpenAI` construction site — task 5.3
of openspec/changes/add-user-llm-byok.

Seven sites construct clients: planner structured + legacy, executor
llm/react/re-prompt, the detached title refinement, and preference
extraction. Each must receive `(base_url, model)` from the run's
override when present (from Settings otherwise) and its API key from the
request-scoped holder — never from the checkpointed state (D11).

Every case asserts the full triple both with and without an override,
and re-asserts that the state serialization carries no key material.
"""

from __future__ import annotations

import json
from typing import Any, ClassVar

import httpx
from langchain_core.messages import AIMessage
from opentelemetry import trace

from agent import executor as executor_module
from agent import main as main_module
from agent import planner as planner_module
from agent import prefs as prefs_module
from agent.config import Settings
from agent.executor import _reprompt_for_corrected_call, _run_llm, _run_react
from agent.llm_effective import OverrideSecrets
from agent.planner import _build_structured_client, _emit_legacy_plan
from agent.prefs import extract_preferences
from agent.state import new_state
from agent.types import (
    ChatMessage,
    LLMOverrideView,
    PlanStep,
    StepKind,
    TavilyOverrideView,
)

_HOLDER_KEY = "holder-key-not-in-state"
_DEPLOYMENT_KEY = "deployment-key"
_OVERRIDE = LLMOverrideView(
    handle="handle-1", base_url="http://user-llm.test/v1", model="user-model"
)
_PLAN_JSON = json.dumps(
    {"steps": [{"id": "s1", "kind": "llm", "prompt": "answer"}]}
)


class _RecordingLlm:
    instances: ClassVar[list[dict]] = []
    canned: ClassVar[str] = "answer"
    tool_calls: ClassVar[list[dict] | None] = None

    def __init__(self, **kwargs) -> None:
        type(self).instances.append(kwargs)

    @classmethod
    def reset(cls, *, canned: str = "answer", tool_calls=None) -> None:
        cls.instances = []
        cls.canned = canned
        cls.tool_calls = tool_calls

    async def ainvoke(self, messages, config=None):
        extra = {}
        if type(self).tool_calls:
            extra["tool_calls"] = [dict(tc) for tc in type(self).tool_calls]
        return AIMessage(content=type(self).canned, **extra)

    def bind(self, **kwargs):
        return self

    def bind_tools(self, schemas):
        return self

    def __or__(self, other):
        return self


class _Budget:
    class _Decision:
        decision = "ok"

    async def check_and_increment(self, user_id, tokens, tracer):
        return self._Decision()


class _Tools:
    async def list_tools(self):
        return []

    async def call(self, name, arguments):
        return {}


class _TitleStore:
    def __init__(self) -> None:
        self.titles: list[tuple] = []

    async def set_title(self, session_id, user_id, title):
        self.titles.append((session_id, user_id, title))


def _settings(**overrides) -> Settings:
    base = {
        "_env_file": None,
        "llm_base_url": "http://deployment-llm.test/v1",
        "llm_api_key": _DEPLOYMENT_KEY,
        "llm_model": "deployment-model",
        "llm_planner_model": "planner-model",
    }
    base.update(overrides)
    return Settings(**base)


def _state(*, view: LLMOverrideView | None = None, model: str | None = None):
    return new_state(
        user_id=1,
        run_id="run-1",
        messages=[ChatMessage(role="user", content="hi")],
        model=model,
        llm_override=view,
        tavily_override=(
            TavilyOverrideView(handle="handle-1", base_url="http://tavily.test")
            if view
            else None
        ),
    )


def _secrets(view: LLMOverrideView | None) -> OverrideSecrets | None:
    if view is None:
        return None
    return OverrideSecrets(handle=view.handle, llm_api_key=_HOLDER_KEY)


def _secret_value(value: Any) -> str:
    if hasattr(value, "get_secret_value"):
        return value.get_secret_value()
    return value


def _assert_credentials(
    kwargs: dict, *, base_url: str, model: str, api_key: str
) -> None:
    assert kwargs["base_url"] == base_url
    assert kwargs["model"] == model
    assert _secret_value(kwargs["api_key"]) == api_key
    if base_url == _OVERRIDE.base_url:
        assert isinstance(kwargs.get("http_async_client"), httpx.AsyncClient), (
            "a user-supplied endpoint must carry the SSRF-guarded client"
        )
    else:
        assert "http_async_client" not in kwargs, (
            "a deployment endpoint must not carry the guard"
        )


def _assert_state_has_no_key(state: dict) -> None:
    payload = json.dumps(
        {
            key: value.model_dump() if hasattr(value, "model_dump") else value
            for key, value in state.items()
        },
        default=str,
    )
    assert _HOLDER_KEY not in payload
    assert "api_key" not in payload


async def test_planner_structured_client_uses_the_override(monkeypatch):
    monkeypatch.setattr(planner_module, "ChatOpenAI", _RecordingLlm)
    _RecordingLlm.reset()
    state = _state(view=_OVERRIDE)

    _build_structured_client(_settings(), state, _secrets(_OVERRIDE))

    _assert_credentials(
        _RecordingLlm.instances[0],
        base_url="http://user-llm.test/v1",
        model="user-model",
        api_key=_HOLDER_KEY,
    )
    _assert_state_has_no_key(state)


async def test_planner_structured_client_without_override_uses_settings(monkeypatch):
    monkeypatch.setattr(planner_module, "ChatOpenAI", _RecordingLlm)
    _RecordingLlm.reset()
    state = _state()

    _build_structured_client(_settings(), state, None)

    _assert_credentials(
        _RecordingLlm.instances[0],
        base_url="http://deployment-llm.test/v1",
        model="planner-model",
        api_key=_DEPLOYMENT_KEY,
    )
    _assert_state_has_no_key(state)


async def test_planner_legacy_client_uses_the_override(monkeypatch):
    monkeypatch.setattr(planner_module, "ChatOpenAI", _RecordingLlm)
    _RecordingLlm.reset(canned=_PLAN_JSON)
    state = _state(view=_OVERRIDE)

    await _emit_legacy_plan(
        state,
        _settings(),
        trace.get_tracer("test"),
        secrets=_secrets(_OVERRIDE),
    )

    _assert_credentials(
        _RecordingLlm.instances[0],
        base_url="http://user-llm.test/v1",
        model="user-model",
        api_key=_HOLDER_KEY,
    )
    _assert_state_has_no_key(state)


async def test_planner_legacy_client_without_override_uses_settings(monkeypatch):
    monkeypatch.setattr(planner_module, "ChatOpenAI", _RecordingLlm)
    _RecordingLlm.reset(canned=_PLAN_JSON)
    state = _state()

    await _emit_legacy_plan(
        state, _settings(), trace.get_tracer("test"), secrets=None
    )

    _assert_credentials(
        _RecordingLlm.instances[0],
        base_url="http://deployment-llm.test/v1",
        model="planner-model",
        api_key=_DEPLOYMENT_KEY,
    )


async def test_executor_llm_step_uses_the_override(monkeypatch):
    monkeypatch.setattr(executor_module, "ChatOpenAI", _RecordingLlm)
    _RecordingLlm.reset()
    state = _state(view=_OVERRIDE)

    with trace.get_tracer("test").start_as_current_span("x") as span:
        await _run_llm(
            state,
            PlanStep(id="s1", kind=StepKind.llm, prompt="answer"),
            _settings(),
            trace.get_tracer("test"),
            _Budget(),
            span,
            secrets=_secrets(_OVERRIDE),
        )

    _assert_credentials(
        _RecordingLlm.instances[0],
        base_url="http://user-llm.test/v1",
        model="user-model",
        api_key=_HOLDER_KEY,
    )
    _assert_state_has_no_key(state)


async def test_executor_llm_step_without_override_uses_settings(monkeypatch):
    monkeypatch.setattr(executor_module, "ChatOpenAI", _RecordingLlm)
    _RecordingLlm.reset()
    state = _state()

    with trace.get_tracer("test").start_as_current_span("x") as span:
        await _run_llm(
            state,
            PlanStep(id="s1", kind=StepKind.llm, prompt="answer"),
            _settings(),
            trace.get_tracer("test"),
            _Budget(),
            span,
        )

    _assert_credentials(
        _RecordingLlm.instances[0],
        base_url="http://deployment-llm.test/v1",
        model="deployment-model",
        api_key=_DEPLOYMENT_KEY,
    )


async def test_executor_react_step_uses_the_override(monkeypatch):
    monkeypatch.setattr(executor_module, "ChatOpenAI", _RecordingLlm)
    _RecordingLlm.reset()
    state = _state(view=_OVERRIDE)

    with trace.get_tracer("test").start_as_current_span("x") as span:
        await _run_react(
            state,
            PlanStep(
                id="s1", kind=StepKind.react, prompt="answer", max_iterations=1
            ),
            _settings(),
            trace.get_tracer("test"),
            _Budget(),
            _Tools(),
            span,
            secrets=_secrets(_OVERRIDE),
        )

    _assert_credentials(
        _RecordingLlm.instances[0],
        base_url="http://user-llm.test/v1",
        model="user-model",
        api_key=_HOLDER_KEY,
    )
    _assert_state_has_no_key(state)


async def test_executor_react_step_without_override_uses_settings(monkeypatch):
    monkeypatch.setattr(executor_module, "ChatOpenAI", _RecordingLlm)
    _RecordingLlm.reset()
    state = _state()

    with trace.get_tracer("test").start_as_current_span("x") as span:
        await _run_react(
            state,
            PlanStep(
                id="s1", kind=StepKind.react, prompt="answer", max_iterations=1
            ),
            _settings(),
            trace.get_tracer("test"),
            _Budget(),
            _Tools(),
            span,
        )

    _assert_credentials(
        _RecordingLlm.instances[0],
        base_url="http://deployment-llm.test/v1",
        model="deployment-model",
        api_key=_DEPLOYMENT_KEY,
    )


async def test_executor_reprompt_uses_the_override(monkeypatch):
    monkeypatch.setattr(executor_module, "ChatOpenAI", _RecordingLlm)
    _RecordingLlm.reset(
        canned="", tool_calls=[{"id": "c1", "name": "list_feeds", "args": {}}]
    )
    state = _state(view=_OVERRIDE)

    result = await _reprompt_for_corrected_call(
        state,
        step_prompt="do it",
        tool_name="list_feeds",
        args={},
        error_text="bad args",
        base_messages=None,
        model_tool_call_id=None,
        carry=[],
        settings=_settings(),
        tracer=trace.get_tracer("test"),
        budget=_Budget(),
        tools=_Tools(),
        secrets=_secrets(_OVERRIDE),
    )

    assert result is not None
    _assert_credentials(
        _RecordingLlm.instances[0],
        base_url="http://user-llm.test/v1",
        model="user-model",
        api_key=_HOLDER_KEY,
    )
    _assert_state_has_no_key(state)


async def test_executor_reprompt_without_override_uses_settings(monkeypatch):
    monkeypatch.setattr(executor_module, "ChatOpenAI", _RecordingLlm)
    _RecordingLlm.reset(
        canned="", tool_calls=[{"id": "c1", "name": "list_feeds", "args": {}}]
    )
    state = _state()

    await _reprompt_for_corrected_call(
        state,
        step_prompt="do it",
        tool_name="list_feeds",
        args={},
        error_text="bad args",
        base_messages=None,
        model_tool_call_id=None,
        carry=[],
        settings=_settings(),
        tracer=trace.get_tracer("test"),
        budget=_Budget(),
        tools=_Tools(),
    )

    _assert_credentials(
        _RecordingLlm.instances[0],
        base_url="http://deployment-llm.test/v1",
        model="deployment-model",
        api_key=_DEPLOYMENT_KEY,
    )


async def test_title_refinement_uses_the_override(monkeypatch):
    monkeypatch.setattr(main_module, "ChatOpenAI", _RecordingLlm)
    _RecordingLlm.reset(canned="refined title")
    store = _TitleStore()

    await main_module._refine_session_title(
        store,
        user_id=1,
        session_id="run-1",
        user_text="hi",
        assistant_text="hello",
        settings=_settings(),
        llm_override=_OVERRIDE,
        secrets=_secrets(_OVERRIDE),
    )

    _assert_credentials(
        _RecordingLlm.instances[0],
        base_url="http://user-llm.test/v1",
        model="user-model",
        api_key=_HOLDER_KEY,
    )
    assert store.titles


async def test_title_refinement_without_override_uses_settings(monkeypatch):
    monkeypatch.setattr(main_module, "ChatOpenAI", _RecordingLlm)
    _RecordingLlm.reset(canned="refined title")
    store = _TitleStore()

    await main_module._refine_session_title(
        store,
        user_id=1,
        session_id="run-1",
        user_text="hi",
        assistant_text="hello",
        settings=_settings(),
    )

    _assert_credentials(
        _RecordingLlm.instances[0],
        base_url="http://deployment-llm.test/v1",
        model="planner-model",
        api_key=_DEPLOYMENT_KEY,
    )
    assert store.titles


async def test_prefs_extraction_uses_the_override(monkeypatch):
    monkeypatch.setattr(prefs_module, "ChatOpenAI", _RecordingLlm)
    _RecordingLlm.reset(canned="[]")

    await extract_preferences(
        "I like tea",
        settings=_settings(),
        llm_override=_OVERRIDE,
        secrets=_secrets(_OVERRIDE),
    )

    _assert_credentials(
        _RecordingLlm.instances[0],
        base_url="http://user-llm.test/v1",
        model="user-model",
        api_key=_HOLDER_KEY,
    )


async def test_prefs_extraction_without_override_uses_settings(monkeypatch):
    monkeypatch.setattr(prefs_module, "ChatOpenAI", _RecordingLlm)
    _RecordingLlm.reset(canned="[]")

    await extract_preferences("I like tea", settings=_settings())

    _assert_credentials(
        _RecordingLlm.instances[0],
        base_url="http://deployment-llm.test/v1",
        model="planner-model",
        api_key=_DEPLOYMENT_KEY,
    )
