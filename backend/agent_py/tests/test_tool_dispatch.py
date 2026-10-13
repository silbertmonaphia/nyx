"""Dispatcher + gating + retry-funnel integration — openspec/changes/
agent-web-tools tasks 4.1, 4.2, 5.2.

The funnel tests drive the REAL `_dispatch_tool_with_retries` with a
`ToolDispatcher` whose local tool fails like a web tool — proving the
spec's "Local failures keep the existing retry semantics" scenario: a
`ToolError` re-prompts the model, a `ToolUnavailableError` retries with
backoff, both unchanged from the registry-tool paths.
"""

from __future__ import annotations

import logging
from typing import ClassVar

import pytest
from langchain_core.messages import AIMessage
from opentelemetry import trace

from agent import executor as executor_module
from agent.config import Settings
from agent.executor import _dispatch_tool_with_retries
from agent.state import new_state
from agent.tool_dispatch import LocalTool, LocalToolRegistry, ToolDispatcher
from agent.tools_client import ToolError, ToolUnavailableError
from agent.web_tools import build_local_tools

PUBLIC_IP = "93.184.216.34"


def mcp_schema(name: str) -> dict:
    return {
        "type": "function",
        "function": {"name": name, "description": "", "parameters": {}},
    }


def native_schema(name: str) -> dict:
    return {"name": name, "description": "", "inputSchema": {}}


class FakeMcp:
    def __init__(self, schemas=None, fail_list=False, result=None):
        self.schemas = schemas or []
        self.fail_list = fail_list
        self.result = result if result is not None else {"ok": True}
        self.calls: list[tuple[str, dict]] = []

    async def list_tools(self):
        if self.fail_list:
            raise RuntimeError("mcp down")
        return list(self.schemas)

    async def call(self, name, arguments):
        self.calls.append((name, arguments))
        return self.result


def local_tool(name: str, dispatch) -> LocalTool:
    return LocalTool(name=name, schema=mcp_schema(name), dispatch=dispatch)


def make_settings(**overrides) -> Settings:
    base = {"_env_file": None, "llm_base_url": "http://stub", "metrics_port": 0}
    base.update(overrides)
    return Settings(**base)


# --- 4.1 dispatcher unit behaviour ------------------------------------------


async def test_merged_list_local_first_then_mcp():
    async def _dispatch(args):
        return {}

    local = LocalToolRegistry([local_tool("web_fetch", _dispatch)])
    mcp = FakeMcp(schemas=[mcp_schema("list_feeds"), mcp_schema("get_feed")])
    dispatcher = ToolDispatcher(mcp=mcp, local=local)

    merged = await dispatcher.list_tools()
    names = [s["function"]["name"] for s in merged]
    assert names == ["web_fetch", "list_feeds", "get_feed"]


async def test_call_routes_local_first_mcp_fallback():
    seen = []

    async def _dispatch(args):
        seen.append(args)
        return {"local": True}

    local = LocalToolRegistry([local_tool("web_fetch", _dispatch)])
    mcp = FakeMcp(result={"mcp": True})
    dispatcher = ToolDispatcher(mcp=mcp, local=local)

    assert await dispatcher.call("web_fetch", {"url": "https://x/"}) == {"local": True}
    assert seen == [{"url": "https://x/"}]
    assert mcp.calls == []  # local name never touches MCP

    assert await dispatcher.call("list_feeds", {}) == {"mcp": True}
    assert mcp.calls == [("list_feeds", {})]


async def test_collision_local_wins_mcp_dropped_and_warned_once(caplog):
    async def _dispatch(args):
        return {}

    local = LocalToolRegistry([local_tool("web_fetch", _dispatch)])
    mcp = FakeMcp(schemas=[mcp_schema("web_fetch"), mcp_schema("list_feeds")])
    dispatcher = ToolDispatcher(mcp=mcp, local=local)

    with caplog.at_level(logging.WARNING, logger="nyx.agent.tool_dispatch"):
        first = await dispatcher.list_tools()
        second = await dispatcher.list_tools()

    for merged in (first, second):
        names = [s["function"]["name"] for s in merged]
        assert names == ["web_fetch", "list_feeds"]

    collision_records = [r for r in caplog.records if "web_fetch" in r.message]
    assert len(collision_records) == 1  # warned once per run, not per call


async def test_mcp_outage_degrades_to_local_only(caplog):
    async def _dispatch(args):
        return {}

    local = LocalToolRegistry([local_tool("web_fetch", _dispatch)])
    dispatcher = ToolDispatcher(mcp=FakeMcp(fail_list=True), local=local)

    with caplog.at_level(logging.WARNING, logger="nyx.agent.tool_dispatch"):
        merged = await dispatcher.list_tools()
    assert [s["function"]["name"] for s in merged] == ["web_fetch"]
    assert any("agent-native tools only" in r.message for r in caplog.records)


async def test_mcp_outage_without_local_tools_is_empty(caplog):
    dispatcher = ToolDispatcher(mcp=FakeMcp(fail_list=True), local=LocalToolRegistry())
    with caplog.at_level(logging.WARNING, logger="nyx.agent.tool_dispatch"):
        assert await dispatcher.list_tools() == []


async def test_native_mcp_schema_shapes_supported():
    dispatcher = ToolDispatcher(
        mcp=FakeMcp(schemas=[native_schema("list_feeds")]), local=LocalToolRegistry()
    )
    merged = await dispatcher.list_tools()
    assert merged == [native_schema("list_feeds")]


def test_duplicate_local_registration_refused():
    async def _dispatch(args):
        return {}

    registry = LocalToolRegistry([local_tool("web_fetch", _dispatch)])
    with pytest.raises(ValueError, match="duplicate"):
        registry.register(local_tool("web_fetch", _dispatch))


# --- 5.2 gating -------------------------------------------------------------


async def test_disabled_flags_leave_merged_list_identical_to_mcp():
    """Spec scenario "Default configuration changes nothing": with both
    flags off, list_tools() returns exactly the MCP registry list."""
    settings = make_settings(web_fetch_enabled=False, web_search_enabled=False)
    local = LocalToolRegistry(build_local_tools(settings))
    mcp_schemas = [mcp_schema("list_feeds"), mcp_schema("get_feed")]
    dispatcher = ToolDispatcher(mcp=FakeMcp(schemas=mcp_schemas), local=local)
    assert await dispatcher.list_tools() == mcp_schemas


async def test_enabled_flags_register_tools():
    settings = make_settings(
        web_fetch_enabled=True, web_search_enabled=True, brave_api_key="k"
    )
    local = LocalToolRegistry(build_local_tools(settings))
    dispatcher = ToolDispatcher(mcp=FakeMcp(schemas=[]), local=local)
    names = [s["function"]["name"] for s in await dispatcher.list_tools()]
    assert names == ["web_fetch", "web_search"]


# --- 4.2 retry-funnel integration -------------------------------------------


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


def _state():
    return new_state(user_id=1, run_id="r", messages=[], model="stub")


@pytest.fixture
def tracer():
    return trace.get_tracer("test-tool-dispatch")


@pytest.fixture(autouse=True)
def _patch_llm(monkeypatch):
    monkeypatch.setattr(executor_module, "ChatOpenAI", _ScriptedLlm)
    _ScriptedLlm.responses = []
    yield


async def test_web_tool_arg_error_drives_reprompt_path(tracer, monkeypatch):
    """A local tool raising ToolError (guard refusal) must route to the
    model re-prompt path: the model gets a corrected call dispatched."""
    attempts = []

    async def _dispatch(args):
        attempts.append(args)
        if len(attempts) == 1:
            raise ToolError("web_fetch: target is not public")
        return {"url": args["url"], "text": "fetched"}

    local = LocalToolRegistry([local_tool("web_fetch", _dispatch)])
    dispatcher = ToolDispatcher(mcp=FakeMcp(), local=local)
    _ScriptedLlm.responses = [
        AIMessage(
            content="",
            tool_calls=[
                {"id": "c2", "name": "web_fetch", "args": {"url": "https://good.test/"}}
            ],
        )
    ]

    state = _state()
    with tracer.start_as_current_span("x") as span:
        ok, result, _content = await _dispatch_tool_with_retries(
            state,
            step_id="s1",
            tool_name="web_fetch",
            args={"url": "http://internal.test/"},
            tools=dispatcher,
            settings=_settings(),
            tracer=tracer,
            budget=_StubBudget(),
            span=span,
            base_messages=[],
            step_prompt="fetch it",
            model_tool_call_id="c1",
        )

    assert ok is True
    assert result == {"url": "https://good.test/", "text": "fetched"}
    assert attempts == [
        {"url": "http://internal.test/"},
        {"url": "https://good.test/"},
    ]
    frames = [e for e in state["events_so_far"] if e.kind == "tool_result"]
    assert frames[0].payload["ok"] is False
    assert frames[-1].payload["ok"] is True


async def test_web_tool_transient_error_retries_with_backoff(tracer):
    """A local tool raising ToolUnavailableError (timeout) must retry the
    same call and succeed without any model round-trip."""
    attempts = []

    async def _dispatch(args):
        attempts.append(args)
        if len(attempts) < 3:
            raise ToolUnavailableError("web_fetch: timeout")
        return {"text": "third time lucky"}

    local = LocalToolRegistry([local_tool("web_fetch", _dispatch)])
    dispatcher = ToolDispatcher(mcp=FakeMcp(), local=local)

    state = _state()
    with tracer.start_as_current_span("x") as span:
        ok, result, _content = await _dispatch_tool_with_retries(
            state,
            step_id="s1",
            tool_name="web_fetch",
            args={"url": "https://slow.test/"},
            tools=dispatcher,
            settings=_settings(),
            tracer=tracer,
            budget=_StubBudget(),
            span=span,
        )

    assert ok is True
    assert result == {"text": "third time lucky"}
    assert len(attempts) == 3
    assert _ScriptedLlm.responses == []  # no re-prompt on the transient path
    retry_frames = [
        e for e in state["events_so_far"] if e.kind == "tool_result"
    ]
    assert retry_frames[-1].payload["retry_count"] == 2


async def test_tool_step_reaches_local_registry_through_run_tool(tracer):
    """Executor-level routing: a plan `tool` step named web_fetch must hit
    the agent-native registry through the real `_run_tool` (review coverage
    gap — the wire fixture uses a hand-rolled stub, bypassing the
    dispatcher)."""
    from agent.executor import _run_tool
    from agent.types import Plan, PlanStep

    async def _dispatch(args):
        return {
            "url": args["url"],
            "status": 200,
            "text": '<untrusted_web_content source="u">\nfetched\n</untrusted_web_content>',
        }

    local = LocalToolRegistry([local_tool("web_fetch", _dispatch)])
    dispatcher = ToolDispatcher(mcp=FakeMcp(), local=local)
    state = _state()
    step = PlanStep(
        id="s1", kind="tool", tool="web_fetch", args={"url": "https://docs.test/"}
    )
    state["plan"] = Plan(steps=[step])

    with tracer.start_as_current_span("x") as span:
        await _run_tool(
            state,
            step,
            settings=_settings(),
            tracer=tracer,
            budget=_StubBudget(),
            tools=dispatcher,
            span=span,
        )

    assert state["step_outputs"]["s1"]["status"] == 200
    assert "fetched" in state["step_outputs"]["s1"]["text"]
    assert dispatcher._mcp.calls == []  # never touched MCP
