from __future__ import annotations

import httpx
import pytest
from pydantic import SecretStr

from agent.llm_client import build_chat_model, guarded_async_client
from agent.llm_effective import OverrideSecrets
from agent.net_guard import WebUrlError
from tests.test_web_tools import make_settings


class _Ctor:
    def __init__(self) -> None:
        self.kwargs: dict = {}

    def __call__(self, **kwargs):
        self.kwargs = kwargs
        return self


def _build(*, user_supplied: bool, settings=None, **overrides):
    ctor = _Ctor()
    build_chat_model(
        ctor,
        base_url="https://llm.example.test/v1",
        api_key="sk-test",
        model="m",
        user_supplied=user_supplied,
        settings=settings or make_settings(),
        **overrides,
    )
    return ctor.kwargs


def test_user_supplied_endpoint_gets_the_guarded_client():
    kwargs = _build(user_supplied=True)
    client = kwargs["http_async_client"]
    assert isinstance(client, httpx.AsyncClient)


def test_deployment_endpoint_is_not_guarded():
    assert "http_async_client" not in _build(user_supplied=False)


def test_byok_allow_private_lifts_the_guard():
    settings = make_settings(byok_allow_private=True)
    assert "http_async_client" not in _build(user_supplied=True, settings=settings)


def test_caller_supplied_client_is_not_replaced():
    sentinel = httpx.AsyncClient()
    kwargs = _build(user_supplied=True, http_async_client=sentinel)
    assert kwargs["http_async_client"] is sentinel


def test_api_key_shape_is_preserved():
    secret = SecretStr("sk-test")
    ctor = _Ctor()
    build_chat_model(
        ctor,
        base_url="https://llm.example.test/v1",
        api_key=secret,
        model="m",
        user_supplied=False,
        settings=make_settings(),
    )
    assert ctor.kwargs["api_key"] is secret


def test_guarded_client_keeps_environment_proxy_mounts(monkeypatch):
    monkeypatch.setenv("HTTP_PROXY", "http://127.0.0.1:9")
    monkeypatch.setenv("HTTPS_PROXY", "http://127.0.0.1:9")
    monkeypatch.setenv("NO_PROXY", "")

    client = guarded_async_client()
    assert client._mounts, "env proxies must stay mounted"
    assert client.follow_redirects is True


def _private_resolver(address: str):
    async def _resolve(host: str, port: int) -> list[str]:
        return [address]

    return _resolve


async def test_guarded_client_refuses_a_private_endpoint_without_network():
    ctor = _Ctor()
    build_chat_model(
        ctor,
        base_url="http://127.0.0.1:1/v1",
        api_key="sk-test",
        model="m",
        user_supplied=True,
        settings=make_settings(),
    )
    from langchain_openai import ChatOpenAI

    client = ChatOpenAI(
        base_url="http://127.0.0.1:1/v1",
        api_key="sk-test",
        model="m",
        temperature=0,
        http_async_client=ctor.kwargs["http_async_client"],
    )
    with pytest.raises(WebUrlError):
        await client.ainvoke("hello")


async def test_executor_step_surfaces_the_guard_as_a_clean_failure(monkeypatch):
    from langchain_openai import ChatOpenAI as RealChatOpenAI
    from opentelemetry import trace as otel_trace

    import agent.executor as executor_module
    from agent.executor import _run_llm, _StepRuntimeError
    from agent.state import new_state
    from agent.types import ChatMessage, LLMOverrideView, Plan, PlanStep

    monkeypatch.setattr(executor_module, "ChatOpenAI", RealChatOpenAI)

    class _Budget:
        class _D:
            decision = "ok"

        async def check_and_increment(self, user_id, tokens, tracer):
            return self._D()

    state = new_state(
        user_id=1,
        run_id="guard-run",
        messages=[ChatMessage(role="user", content="hi")],
        model="m",
    )
    state["llm_override"] = LLMOverrideView(
        handle="h", base_url="http://127.0.0.1:1/v1", model="m"
    )
    state["plan"] = Plan(steps=[PlanStep(id="s1", kind="llm", prompt="Answer.")])
    tracer = otel_trace.get_tracer("test-llm-client")

    with pytest.raises(_StepRuntimeError) as excinfo, tracer.start_as_current_span("x") as span:
        await _run_llm(
            state,
            PlanStep(id="s1", kind="llm", prompt="Answer."),
            make_settings(),
            tracer,
            _Budget(),
            span,
            secrets=OverrideSecrets(handle="h", llm_api_key="sk-test"),
        )
    assert "WebUrlError" in str(excinfo.value)


async def test_planner_reports_the_guard_as_planner_failed(monkeypatch):
    from langchain_openai import ChatOpenAI as RealChatOpenAI
    from opentelemetry import trace as otel_trace

    import agent.planner as planner_module
    from agent.planner import plan_node
    from agent.state import new_state
    from agent.types import ChatMessage, Event, LLMOverrideView

    monkeypatch.setattr(planner_module, "ChatOpenAI", RealChatOpenAI)

    state = new_state(
        user_id=1,
        run_id="guard-plan-run",
        messages=[ChatMessage(role="user", content="hi")],
        model="m",
    )
    state["llm_override"] = LLMOverrideView(
        handle="h", base_url="http://127.0.0.1:1/v1", model="m"
    )
    tracer = otel_trace.get_tracer("test-llm-client")

    out = await plan_node(
        state,
        make_settings(),
        tracer,
        secrets=OverrideSecrets(handle="h", llm_api_key="sk-test"),
    )
    errors = [e for e in out["events_so_far"] if isinstance(e, Event) and e.kind == "error"]
    assert errors, out["events_so_far"]
    assert errors[-1].payload["code"] == "planner_failed"
