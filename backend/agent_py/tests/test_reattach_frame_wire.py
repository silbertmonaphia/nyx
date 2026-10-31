"""Producer-side pin for the re-attach SSE bytes (agent-stream-resilience).

The frontend parser test (chatService.test.ts) consumes the checked-in
fixture `frontend/src/features/chat/services/__fixtures__/reattach_stream.sse`
through its real parser. THIS test is the producer half of that contract:
it encodes every frame through the real emitter (`main._sse_encode`, the
real `NoteEvent` / `Event` models and the real replay projection) and
asserts the fixture is byte-identical — so a producer-side rename, a type
change, or a serialization-order change fails HERE, instead of silently
degrading the re-attach view.

The fixture also carries the keep-alive comment frame in the middle of
the sequence: the SPA must skip it and still see every event frame.

Regenerate the fixture after a deliberate wire change (from backend/agent_py):
    PYTHON_AGENT_UPDATE_WIRE_FIXTURES=1 \
        .venv/bin/python -m pytest tests/test_reattach_frame_wire.py -q
…and update the TS test's expected literals in the same change.
"""

from __future__ import annotations

import asyncio
import os
from pathlib import Path

from agent.main import _sse_encode
from agent.types import Event, NoteEvent

FIXTURE = (
    Path(__file__).resolve().parents[3]
    / "frontend"
    / "src"
    / "features"
    / "chat"
    / "services"
    / "__fixtures__"
    / "reattach_stream.sse"
)

RUN_ID = "6b1f0c2e-7d3a-4e5f-8a9b-0c1d2e3f4a5b"

NOTE_FRAME = _sse_encode(
    Event(
        kind="note",
        payload=NoteEvent(
            text="reattached",
            kind="reattached",
            run_id=RUN_ID,
        ).model_dump(),
    )
)

PLAN_FRAME = _sse_encode(
    Event(
        kind="plan",
        payload={
            "steps": [
                {"id": "step_1", "kind": "tool", "tool": "list_feeds", "args": {}}
            ]
        },
    )
)

STEP_START_FRAME = _sse_encode(
    Event(kind="step_start", payload={"step_id": "step_1", "kind": "tool"})
)

TOOL_CALL_FRAME = _sse_encode(
    Event(
        kind="tool_call",
        payload={
            "step_id": "step_1",
            "call_id": "step_1:replay",
            "name": "list_feeds",
            "args": {},
        },
    )
)

TOOL_RESULT_FRAME = _sse_encode(
    Event(
        kind="tool_result",
        payload={
            "step_id": "step_1",
            "call_id": "step_1:replay",
            "ok": True,
            "result": {"items": []},
            "retry_count": 0,
        },
    )
)

STEP_END_FRAME = _sse_encode(
    Event(
        kind="step_end",
        payload={"step_id": "step_1", "status": "ok", "reason": ""},
    )
)

DELTA_FRAME = _sse_encode(
    Event(kind="delta", payload={"delta": "re-attached answer"})
)

DONE_FRAME = _sse_encode(
    Event(
        kind="done",
        payload={
            "usage": {
                "prompt_tokens": 12,
                "completion_tokens": 7,
                "total_tokens": 19,
            }
        },
    )
)

KEEPALIVE_FRAME = b": keep-alive\n\n"

STREAM = (
    NOTE_FRAME
    + PLAN_FRAME
    + STEP_START_FRAME
    + TOOL_CALL_FRAME
    + TOOL_RESULT_FRAME
    + STEP_END_FRAME
    + KEEPALIVE_FRAME
    + DELTA_FRAME
    + DONE_FRAME
    + b"data: [DONE]\n\n"
)


def test_reattach_fixture_matches_emitted_bytes():
    if os.environ.get("PYTHON_AGENT_UPDATE_WIRE_FIXTURES"):
        FIXTURE.write_bytes(STREAM)

    assert FIXTURE.exists(), f"missing fixture: {FIXTURE}"
    assert FIXTURE.read_bytes() == STREAM, (
        "reattach fixture drifted from the emitter — regenerate it with "
        "PYTHON_AGENT_UPDATE_WIRE_FIXTURES=1 and update the TS test's "
        "expected literals in the same change"
    )


def test_keepalive_comment_frame_shape():
    """The keep-alive is an SSE comment: no event field, no data field —
    a parser that skips comment lines never sees it as an event."""
    assert KEEPALIVE_FRAME == b": keep-alive\n\n"
    assert b"event:" not in KEEPALIVE_FRAME
    assert b"data:" not in KEEPALIVE_FRAME


def test_replay_projection_matches_the_emitted_frames():
    """The projection the re-attach endpoint opens with is the same
    sequence the fixture pins — the session's folded trace rebuilt into
    wire frames."""
    from agent.config import Settings
    from agent.main import _RunSession
    from agent.types import ChatMessage, ChatRequest

    async def _build() -> list[Event]:
        session = _RunSession(
            run_id=RUN_ID,
            user_id=1,
            request_id=None,
            chat_req=ChatRequest(
                messages=[ChatMessage(role="user", content="hi")]
            ),
            settings=Settings(_env_file=None, llm_base_url="http://x/v1"),
            app=None,  # type: ignore[arg-type]
            run_metadata=None,
            chat_store=None,
            prefs_store=None,
            sink=None,  # type: ignore[arg-type]
            queue=asyncio.Queue(),
            state=None,  # type: ignore[arg-type]
            resumed=False,
            secrets=None,
            llm_view=None,
            title_written=False,
            persist_run_state=False,
        )
        session.collect(
            Event(
                kind="plan",
                payload={
                    "steps": [
                        {
                            "id": "step_1",
                            "kind": "tool",
                            "tool": "list_feeds",
                            "args": {},
                        }
                    ]
                },
            )
        )
        session.collect(
            Event(kind="step_start", payload={"step_id": "step_1", "kind": "tool"})
        )
        session.collect(
            Event(
                kind="tool_call",
                payload={
                    "step_id": "step_1",
                    "call_id": "step_1:replay",
                    "name": "list_feeds",
                    "args": {},
                },
            )
        )
        session.collect(
            Event(
                kind="tool_result",
                payload={
                    "step_id": "step_1",
                    "call_id": "step_1:replay",
                    "ok": True,
                    "result": {"items": []},
                },
            )
        )
        session.collect(
            Event(
                kind="step_end",
                payload={"step_id": "step_1", "status": "ok", "reason": ""},
            )
        )
        return session.replay_frames()

    frames = asyncio.run(_build())
    encoded = b"".join(_sse_encode(f) for f in frames)
    assert encoded == (
        PLAN_FRAME + STEP_START_FRAME + TOOL_CALL_FRAME + TOOL_RESULT_FRAME + STEP_END_FRAME
    ), "the replay projection must reproduce the pinned step frames verbatim"
