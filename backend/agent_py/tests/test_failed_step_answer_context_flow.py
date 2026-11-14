from __future__ import annotations

from typing import ClassVar

import pytest
from langchain_core.messages import AIMessage
from opentelemetry import trace

from agent import executor as executor_module
from agent.approvals import ApprovalGate
from agent.config import Settings
from agent.executor import executor_node
from agent.planner import _enforce_plan_shape, _PlanError
from agent.state import new_state
from agent.types import ChatMessage, Plan, PlanStep

DENIAL_TEXT = (
    "Confirmation required: the approval request expired before a decision. "
    "The `create_feed` tool was not executed."
)


class _StubLlm:
    captured: ClassVar[list] = []

    def __init__(self, *a, **kw):
        pass

    async def ainvoke(self, messages):
        type(self).captured.append(list(messages))
        for message in messages:
            content = message.content if isinstance(message.content, str) else ""
            if "tool was not executed" in content:
                return AIMessage(content=f"创建失败。{DENIAL_TEXT}")
        return AIMessage(content="stub answer")


class _StubBudget:
    class _Decision:
        decision = "ok"

    async def check_and_increment(self, user_id, tokens, tracer):
        return self._Decision()


class _GatedTools:
    async def list_tools(self):
        return []

    async def confirmation_required(self, name):
        return name == "create_feed"

    async def call(self, name, arguments):
        raise AssertionError("denied dispatch must not reach the tool")


class _ExpiredApprovals:
    async def put_intent(self, run_id, approval_id, expires_at):
        return None

    async def wait_for_decision(self, run_id, approval_id, expires_at):
        return "expired"


def _settings() -> Settings:
    return Settings(
        llm_base_url="http://stub",
        metrics_port=0,
        tool_retry_max=2,
        tool_retry_backoff_base_ms=0,
    )


@pytest.fixture(autouse=True)
def _patch_llm(monkeypatch):
    monkeypatch.setattr(executor_module, "ChatOpenAI", _StubLlm)
    _StubLlm.captured = []
    yield
    _StubLlm.captured = []


@pytest.mark.asyncio
async def test_expired_write_answer_then_retry_instruction_replans_the_write():
    steps = [
        PlanStep(
            id="tool_1",
            kind="tool",
            tool="create_feed",
            args={"title": "Flow", "url": "https://example.com/flow.xml"},
        ),
        PlanStep(id="llm_2", kind="llm", prompt="Answer."),
    ]
    state = new_state(
        user_id=1,
        run_id="flow-run",
        messages=[
            ChatMessage(
                role="user",
                content="帮我创建一篇 feed：Flow，https://example.com/flow.xml",
            )
        ],
        model="stub",
    )
    state["plan"] = Plan(steps=steps)

    await executor_node(
        state,
        settings=_settings(),
        tracer=trace.get_tracer("test-flow"),
        budget=_StubBudget(),
        rag=None,
        tools=_GatedTools(),
        approvals=ApprovalGate(store=_ExpiredApprovals(), ttl_seconds=1),
    )

    events = state["events_so_far"]
    assert any(
        event.kind == "tool_result"
        and (event.payload or {}).get("ok") is False
        and "not executed" in ((event.payload or {}).get("error") or "")
        for event in events
    )
    assert any(
        event.kind == "step_end"
        and (event.payload or {}).get("step_id") == "tool_1"
        and (event.payload or {}).get("status") == "failed"
        for event in events
    )

    answer_input = "\n".join(
        message.content for message in _StubLlm.captured[0] if message.content
    )
    assert DENIAL_TEXT in answer_input
    assert "do not present the failed action as successful" in answer_input

    answer_text = "".join(
        (event.payload or {}).get("delta") or ""
        for event in events
        if event.kind == "delta"
    )
    assert "create_feed" in answer_text
    assert "not executed" in answer_text

    retry_messages = [
        ChatMessage(
            role="user",
            content="帮我创建一篇 feed：Flow，https://example.com/flow.xml",
        ),
        ChatMessage(role="assistant", content=answer_text),
        ChatMessage(role="user", content="再试试"),
    ]
    latest = retry_messages[-1].content
    previous = retry_messages[-2].content

    rejected = _enforce_plan_shape(
        Plan(steps=[PlanStep(id="llm_1", kind="llm", prompt="Answer.")]),
        max_steps=10,
        latest_user_message=latest,
        previous_assistant_message=previous,
    )
    assert isinstance(rejected, _PlanError)
    assert "create_feed" in (rejected.reason or "")

    accepted = _enforce_plan_shape(
        Plan(
            steps=[
                PlanStep(id="llm_1", kind="llm", prompt="Compose the feed body."),
                PlanStep(
                    id="tool_1",
                    kind="tool",
                    tool="create_feed",
                    args={
                        "title": "Flow",
                        "url": "https://example.com/flow.xml",
                        "description": "{{step:llm_1}}",
                    },
                ),
                PlanStep(id="llm_2", kind="llm", prompt="Answer."),
            ]
        ),
        max_steps=10,
        latest_user_message=latest,
        previous_assistant_message=previous,
    )
    assert not isinstance(accepted, _PlanError)
