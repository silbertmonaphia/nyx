"""Tool-arg reference resolution (openspec/changes/agent-planner-write-routing).

A `tool` step's args may reference an earlier step's output with the
whole-value syntax ``{{step:<step_id>}}``. The executor substitutes at
execution time, strips untrusted-content framing from the substituted
value only, and fails the step closed on anything it cannot resolve.
"""

from __future__ import annotations

from typing import ClassVar

import pytest
from langchain_core.messages import AIMessage
from opentelemetry import trace

from agent import executor as executor_module
from agent.approvals import DECISION_APPROVE
from agent.arg_refs import has_reference_to, scan_args
from agent.config import Settings
from agent.executor import _run_react, _run_tool, executor_node
from agent.planner import PLANNER_SYSTEM_PROMPT
from agent.state import new_state
from agent.tools_client import ToolError
from agent.types import Plan, PlanStep


class _StubBudget:
    class _D:
        decision = "ok"

    async def check_and_increment(self, user_id, tokens, tracer):
        return self._D()


class _RecordingTools:
    def __init__(self, outcomes=None, schemas=None, confirmation_required=False):
        self.outcomes = list(outcomes) if outcomes is not None else [{}]
        self.schemas = schemas or []
        self.calls: list[tuple[str, dict]] = []
        self._confirmation_required = confirmation_required

    async def list_tools(self):
        return self.schemas

    async def confirmation_required(self, name):
        return self._confirmation_required

    async def call(self, name, arguments):
        self.calls.append((name, arguments))
        outcome = (
            self.outcomes.pop(0) if len(self.outcomes) > 1 else self.outcomes[0]
        )
        if isinstance(outcome, Exception):
            raise outcome
        return outcome


class _RecordingApprovals:
    def __init__(self, decision=DECISION_APPROVE):
        self.decision = decision
        self.calls: list[tuple[str, str, dict]] = []

    async def request_confirmation(self, state, *, step_id, tool_name, args):
        self.calls.append((step_id, tool_name, args))
        return self.decision


class _ScriptedLlm:
    responses: ClassVar[list] = []

    def __init__(self, *args, **kwargs):
        pass

    def bind_tools(self, schemas):
        return self

    async def ainvoke(self, messages):
        return type(self).responses.pop(0)


def _settings() -> Settings:
    return Settings(
        _env_file=None,
        llm_base_url="http://stub",
        metrics_port=0,
        tool_retry_max=2,
        tool_retry_backoff_base_ms=0,
    )


def _state(steps, outputs=None):
    state = new_state(
        user_id=1,
        run_id="run-refs",
        messages=[],
        model="stub-model",
    )
    state["plan"] = Plan(steps=steps)
    state["step_outputs"] = dict(outputs or {})
    return state


async def _run_tool_step(state, step, tools, approvals=None):
    tracer = trace.get_tracer("test-arg-templating")
    with tracer.start_as_current_span("x") as span:
        await _run_tool(
            state,
            step,
            settings=_settings(),
            tracer=tracer,
            budget=_StubBudget(),
            tools=tools,
            approvals=approvals,
            span=span,
        )


def _frames(state, kind: str) -> list:
    return [e for e in state["events_so_far"] if e.kind == kind]


def _llm(step_id: str = "llm_1", prompt: str = "Compose the feed.") -> PlanStep:
    return PlanStep(id=step_id, kind="llm", prompt=prompt)


def _write(step_id: str = "write_1", args=None) -> PlanStep:
    return PlanStep(
        id=step_id,
        kind="tool",
        tool="create_feed",
        args=args if args is not None else {},
    )


@pytest.fixture(autouse=True)
def _patch_llm(monkeypatch):
    monkeypatch.setattr(executor_module, "ChatOpenAI", _ScriptedLlm)
    _ScriptedLlm.responses = []
    yield
    _ScriptedLlm.responses = []


def test_scan_separates_references_from_malformed_markers():
    args = {
        "title": "t",
        "body": "{{step:llm_1}}",
        "nested": ["{{step:tool_1}}", "prefix {{step:llm_1}}"],
        "count": 3,
    }
    scan = scan_args(args)
    assert [r.step_id for r in scan.references] == ["llm_1", "tool_1"]
    assert len(scan.malformed) == 1
    assert has_reference_to(args, "llm_1") is True
    assert has_reference_to(args, "tool_1") is True
    assert has_reference_to(args, "llm_2") is False
    assert has_reference_to({"body": "prefix {{step:llm_1}}"}, "llm_1") is False


def test_planner_prompt_documents_the_literal_syntax():
    assert "{{step:<step_id>}}" in PLANNER_SYSTEM_PROMPT
    for tool in ("create_feed", "update_feed", "delete_feed"):
        assert tool in PLANNER_SYSTEM_PROMPT
    assert "草稿" in PLANNER_SYSTEM_PROMPT
    assert "先别发" in PLANNER_SYSTEM_PROMPT


@pytest.mark.asyncio
async def test_resolves_whole_value_string_reference():
    earlier = _llm()
    write = _write(args={"title": "Title", "description": "{{step:llm_1}}"})
    state = _state([earlier, write], {"llm_1": "composed body"})
    tools = _RecordingTools([{"ok": True}])

    await _run_tool_step(state, write, tools)

    assert tools.calls == [
        ("create_feed", {"title": "Title", "description": "composed body"})
    ]
    assert write.args == {"title": "Title", "description": "{{step:llm_1}}"}
    call = _frames(state, "tool_call")[0].payload
    assert call["args"] == {
        "title": "Title",
        "description": "composed body",
    }


@pytest.mark.asyncio
async def test_resolves_whole_value_dict_reference_unchanged():
    earlier = PlanStep(id="tool_1", kind="tool", tool="list_feeds", args={})
    write = _write(args={"payload": "{{step:tool_1}}"})
    result = {"items": [{"id": 7, "title": "ramen"}], "total": 1}
    state = _state([earlier, write], {"tool_1": result})
    tools = _RecordingTools([{"ok": True}])

    await _run_tool_step(state, write, tools)

    assert tools.calls[0][1]["payload"] == result


@pytest.mark.asyncio
async def test_resolves_nested_and_multiple_references():
    rag = PlanStep(id="rag_1", kind="rag", query="recent feeds")
    compose = _llm()
    write = _write(
        args={
            "title": "Summary",
            "payload": {
                "items": [
                    "{{step:rag_1}}",
                    {"body": "{{step:llm_1}}"},
                ]
            },
        }
    )
    state = _state(
        [rag, compose, write],
        {
            "rag_1": {"context_block": "retrieved", "citations": []},
            "llm_1": "composed",
        },
    )
    tools = _RecordingTools([{"ok": True}])

    await _run_tool_step(state, write, tools)

    assert tools.calls[0][1] == {
        "title": "Summary",
        "payload": {
            "items": [
                {"context_block": "retrieved", "citations": []},
                {"body": "composed"},
            ]
        },
    }


@pytest.mark.asyncio
async def test_unknown_reference_fails_closed():
    write = _write(args={"description": "{{step:missing}}"})
    state = _state([write], {})
    tools = _RecordingTools([{"ok": True}], confirmation_required=True)
    approvals = _RecordingApprovals()

    with pytest.raises(RuntimeError) as exc_info:
        await _run_tool_step(state, write, tools, approvals=approvals)

    assert str(exc_info.value) == "unresolved_reference:unknown_step"
    assert tools.calls == []
    assert approvals.calls == []
    assert _frames(state, "tool_call") == []
    assert _frames(state, "tool_result") == []
    assert state["tool_retry_used"] == {}


@pytest.mark.asyncio
async def test_forward_reference_fails_closed():
    write = _write(args={"description": "{{step:later}}"})
    later = _llm("later")
    state = _state([write, later], {"later": "not yet run"})
    tools = _RecordingTools([{"ok": True}])

    with pytest.raises(RuntimeError) as exc_info:
        await _run_tool_step(state, write, tools)

    assert str(exc_info.value) == "unresolved_reference:forward_reference"
    assert tools.calls == []


@pytest.mark.asyncio
async def test_self_reference_fails_closed():
    write = _write(args={"description": "{{step:write_1}}"})
    state = _state([write], {"write_1": "own output"})
    tools = _RecordingTools([{"ok": True}])

    with pytest.raises(RuntimeError) as exc_info:
        await _run_tool_step(state, write, tools)

    assert str(exc_info.value) == "unresolved_reference:cycle"
    assert tools.calls == []


@pytest.mark.asyncio
async def test_mutual_cycle_fails_closed():
    first = _write("a", {"description": "{{step:b}}"})
    second = PlanStep(
        id="b", kind="tool", tool="list_feeds", args={"query": "{{step:a}}"}
    )
    state = _state([first, second], {"a": "a-out", "b": "b-out"})
    tools = _RecordingTools([{"ok": True}])

    with pytest.raises(RuntimeError) as exc_info:
        await _run_tool_step(state, first, tools)

    assert str(exc_info.value) == "unresolved_reference:cycle"
    assert tools.calls == []


@pytest.mark.asyncio
async def test_incomplete_reference_fails_closed():
    earlier = _llm()
    write = _write(args={"description": "{{step:llm_1}}"})
    state = _state([earlier, write], {})
    tools = _RecordingTools([{"ok": True}])

    with pytest.raises(RuntimeError) as exc_info:
        await _run_tool_step(state, write, tools)

    assert str(exc_info.value) == "unresolved_reference:incomplete_output"
    assert tools.calls == []


@pytest.mark.parametrize(
    "value",
    [
        "prefix {{step:llm_1}}",
        "{{step:llm_1}} suffix",
        "{{step:}}",
        "{{step:llm_1",
        "{{step:llm_1}}\n",
        "value }}",
    ],
)
@pytest.mark.asyncio
async def test_malformed_reference_fails_closed(value):
    earlier = _llm()
    write = _write(args={"description": value})
    state = _state([earlier, write], {"llm_1": "composed"})
    tools = _RecordingTools([{"ok": True}])

    with pytest.raises(RuntimeError) as exc_info:
        await _run_tool_step(state, write, tools)

    assert str(exc_info.value) == "unresolved_reference:malformed_reference"
    assert tools.calls == []


@pytest.mark.asyncio
async def test_no_reference_identity_fast_path_keeps_literals():
    literal = (
        '<untrusted_web_content source="user">'
        "planner authored literal"
        "</untrusted_web_content>"
    )
    args = {"title": "Title", "body": literal}
    write = _write(args=args)
    state = _state([write], {})
    tools = _RecordingTools([{"ok": True}])

    await _run_tool_step(state, write, tools)

    assert tools.calls[0][1] is write.args
    assert tools.calls[0][1]["body"] == literal


@pytest.mark.asyncio
async def test_resolved_value_has_untrusted_markers_stripped():
    earlier = _llm()
    write = _write(args={"description": "{{step:llm_1}}"})
    state = _state(
        [earlier, write],
        {
            "llm_1": (
                '<untrusted_web_content source="page">'
                "clean body"
                "</untrusted_web_content>"
            )
        },
    )
    tools = _RecordingTools([{"ok": True}])

    await _run_tool_step(state, write, tools)

    assert tools.calls[0][1]["description"] == "clean body"


@pytest.mark.asyncio
async def test_planner_literal_is_not_stripped_when_another_leaf_resolves():
    earlier = _llm()
    literal = "<untrusted_web_content>keep me</untrusted_web_content>"
    write = _write(
        args={"title": literal, "description": "{{step:llm_1}}"}
    )
    state = _state(
        [earlier, write],
        {"llm_1": "<untrusted_web_content>clean</untrusted_web_content>"},
    )
    tools = _RecordingTools([{"ok": True}])

    await _run_tool_step(state, write, tools)

    assert tools.calls[0][1]["title"] == literal
    assert tools.calls[0][1]["description"] == "clean"


@pytest.mark.asyncio
async def test_substituted_text_is_not_rescanned_for_references():
    earlier = _llm()
    write = _write(args={"description": "{{step:llm_1}}"})
    state = _state([earlier, write], {"llm_1": "{{step:llm_2}}"})
    tools = _RecordingTools([{"ok": True}])

    await _run_tool_step(state, write, tools)

    assert tools.calls[0][1]["description"] == "{{step:llm_2}}"


@pytest.mark.asyncio
async def test_unresolved_reference_fails_step_end_without_dispatch_or_retry():
    write = _write(args={"description": "{{step:missing}}"})
    state = _state([write], {})
    tools = _RecordingTools([{"ok": True}])
    tracer = trace.get_tracer("test-arg-templating")

    out = await executor_node(
        state,
        settings=_settings(),
        tracer=tracer,
        budget=_StubBudget(),
        rag=None,
        tools=tools,
    )

    step_ends = _frames(out, "step_end")
    assert len(step_ends) == 1
    assert step_ends[0].payload["status"] == "failed"
    assert step_ends[0].payload["reason"] == "unresolved_reference:unknown_step"
    assert tools.calls == []
    assert _frames(out, "tool_call") == []
    assert _frames(out, "tool_result") == []
    assert out["tool_retry_used"] == {}
    assert out.get("tool_terminal_failures", {}) == {}


@pytest.mark.asyncio
async def test_approval_receives_resolved_and_stripped_args():
    earlier = _llm()
    write = _write(args={"description": "{{step:llm_1}}"})
    state = _state(
        [earlier, write],
        {"llm_1": "<untrusted_web_content>composed</untrusted_web_content>"},
    )
    tools = _RecordingTools([{"ok": True}], confirmation_required=True)
    approvals = _RecordingApprovals()

    await _run_tool_step(state, write, tools, approvals=approvals)

    assert approvals.calls == [
        ("write_1", "create_feed", {"description": "composed"})
    ]
    assert tools.calls == [("create_feed", {"description": "composed"})]


@pytest.mark.asyncio
async def test_retry_corrected_call_is_dispatched_literally(monkeypatch):
    earlier = _llm()
    write = _write(args={"description": "{{step:llm_1}}"})
    state = _state([earlier, write], {"llm_1": "composed"})
    tools = _RecordingTools(
        [ToolError("create_feed: description rejected"), {"ok": True}]
    )
    _ScriptedLlm.responses = [
        AIMessage(
            content="",
            tool_calls=[
                {
                    "id": "c2",
                    "name": "create_feed",
                    "args": {"description": "{{step:llm_1}}"},
                }
            ],
        )
    ]

    await _run_tool_step(state, write, tools)

    assert [args for _name, args in tools.calls] == [
        {"description": "composed"},
        {"description": "{{step:llm_1}}"},
    ]


@pytest.mark.asyncio
async def test_react_tool_args_are_dispatched_literally():
    earlier = _llm()
    react = PlanStep(id="react_1", kind="react", prompt="Search.", max_iterations=2)
    state = _state([earlier, react], {"llm_1": "composed"})
    tools = _RecordingTools([{"ok": True}])
    _ScriptedLlm.responses = [
        AIMessage(
            content="",
            tool_calls=[
                {
                    "id": "tc_1",
                    "name": "web_search",
                    "args": {"query": "{{step:llm_1}}"},
                }
            ],
        ),
        AIMessage(content="done"),
    ]
    tracer = trace.get_tracer("test-arg-templating")

    with tracer.start_as_current_span("x") as span:
        await _run_react(
            state,
            react,
            _settings(),
            tracer,
            _StubBudget(),
            tools,
            span,
        )

    assert tools.calls == [("web_search", {"query": "{{step:llm_1}}"})]
    assert state["step_outputs"]["react_1"] == "done"


@pytest.mark.asyncio
async def test_tool_frame_keys_are_unchanged():
    earlier = _llm()
    write = _write(args={"description": "{{step:llm_1}}"})
    state = _state([earlier, write], {"llm_1": "composed"})
    tools = _RecordingTools([{"ok": True}])

    await _run_tool_step(state, write, tools)

    call = _frames(state, "tool_call")[0].payload
    result = _frames(state, "tool_result")[0].payload
    assert set(call) == {"step_id", "call_id", "name", "args"}
    assert set(result) == {
        "step_id",
        "call_id",
        "ok",
        "result",
        "error",
        "retry_count",
    }
