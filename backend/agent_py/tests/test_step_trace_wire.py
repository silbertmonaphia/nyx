"""Producer-side pin for the persisted `steps` replay wire.

The frontend parser test consumes the checked-in fixture
`frontend/src/features/chat/services/__fixtures__/step_trace.json`
through the real `chatService.getMessages` parser. THIS test is the
producer half of that contract: it folds the real SSE frames through
`StepTrace`, serializes the rows through the real
`chat_sessions._message_json`, and asserts the fixture is byte-identical
— so a field rename, kind/status change, or ordering drift fails here
instead of silently degrading replay to "no steps".

Regenerate the fixture after a deliberate wire change (from backend/agent_py):
    PYTHON_AGENT_UPDATE_WIRE_FIXTURES=1 \
        .venv/bin/python -m pytest tests/test_step_trace_wire.py -q
…and update the TS test's expected literals in the same change.
"""

from __future__ import annotations

import json
import os
from datetime import UTC, datetime
from pathlib import Path

from agent.api.chat_sessions import _message_json
from agent.chat_store import MessageRow
from agent.step_trace import StepTrace
from agent.types import (
    PlanEvent,
    PlanStep,
    StepEndEvent,
    StepStartEvent,
    ToolCallEvent,
    ToolResultEvent,
)

FIXTURE = (
    Path(__file__).resolve().parents[3]
    / "frontend"
    / "src"
    / "features"
    / "chat"
    / "services"
    / "__fixtures__"
    / "step_trace.json"
)


def _trace() -> list[dict]:
    trace = StepTrace()
    frames = [
        (
            "plan",
            PlanEvent(
                steps=[
                    PlanStep(
                        id="step_1",
                        kind="tool",
                        tool="web_search",
                        args={"query": "rust blog"},
                    ),
                    PlanStep(id="step_2", kind="llm", prompt="summarize"),
                ]
            ).model_dump(),
        ),
        ("step_start", StepStartEvent(step_id="step_1", kind="tool").model_dump()),
        (
            "tool_call",
            ToolCallEvent(
                step_id="step_1",
                call_id="call_1",
                name="web_search",
                args={"query": "rust blog"},
            ).model_dump(),
        ),
        (
            "tool_result",
            ToolResultEvent(
                step_id="step_1",
                call_id="call_1",
                ok=True,
                result="Rust blog feed",
                retry_count=0,
            ).model_dump(),
        ),
        ("step_end", StepEndEvent(step_id="step_1", status="ok").model_dump()),
        ("step_start", StepStartEvent(step_id="step_2", kind="llm").model_dump()),
        ("step_end", StepEndEvent(step_id="step_2", status="ok").model_dump()),
    ]
    for kind, payload in frames:
        trace.fold(kind, payload)
    steps = trace.to_list()
    assert steps is not None
    return steps


TRACE = _trace()
PAYLOAD = {
    "messages": [
        _message_json(
            MessageRow(
                id=42,
                role="user",
                content="summarize my feeds",
                tool_calls=None,
                tokens=0,
                prompt_tokens=None,
                completion_tokens=None,
                created_at=datetime(2026, 10, 18, 12, 0, 0, tzinfo=UTC),
                session_id="2f6c1a94-8d3e-4b57-9c02-7e5a1d8b3f60",
                steps=None,
            )
        ),
        _message_json(
            MessageRow(
                id=43,
                role="assistant",
                content="Here is the summary.",
                tool_calls=[
                    {
                        "step_id": "step_1",
                        "call_id": "call_1",
                        "name": "web_search",
                        "args": {"query": "rust blog"},
                    }
                ],
                tokens=1128,
                prompt_tokens=994,
                completion_tokens=134,
                created_at=datetime(2026, 10, 18, 12, 0, 5, tzinfo=UTC),
                session_id="2f6c1a94-8d3e-4b57-9c02-7e5a1d8b3f60",
                steps=TRACE,
            )
        ),
    ]
}
WIRE = (json.dumps(PAYLOAD, indent=2, ensure_ascii=False) + "\n").encode()


def test_step_trace_fixture_matches_persisted_wire():
    if os.environ.get("PYTHON_AGENT_UPDATE_WIRE_FIXTURES"):
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_bytes(WIRE)

    assert FIXTURE.exists(), f"missing fixture: {FIXTURE}"
    assert FIXTURE.read_bytes() == WIRE, (
        "step trace fixture drifted from the serializer — regenerate it "
        "with PYTHON_AGENT_UPDATE_WIRE_FIXTURES=1 and update the TS test's "
        "expected literals in the same change"
    )
