"""Producer-side pin for the id-bearing SSE bytes (agent-stream-frame-replay).

The frontend parser test (chatService.test.ts) consumes the checked-in
fixture `frontend/src/features/chat/services/__fixtures__/frame_id_stream.sse`
through its real parser. THIS test is the producer half of that contract:
it stamps the ids through the real emitter (`_RunSession.encode_event`,
which is the only path that writes an `id:` line) and asserts the fixture
is byte-identical — so a producer-side change to the id format, the
serialization order, or the frame shape fails HERE, instead of silently
degrading the SPA's cursor bookkeeping.

Regenerate the fixture after a deliberate wire change (from backend/agent_py):
    PYTHON_AGENT_UPDATE_WIRE_FIXTURES=1 \
        .venv/bin/python -m pytest tests/test_frame_id_wire.py -q
…and update the TS test's expected literals in the same change.
"""

from __future__ import annotations

import asyncio
import os
import re
from pathlib import Path

from agent.config import Settings
from agent.main import _RunSession
from agent.types import ChatMessage, ChatRequest, Event

FIXTURE = (
    Path(__file__).resolve().parents[3]
    / "frontend"
    / "src"
    / "features"
    / "chat"
    / "services"
    / "__fixtures__"
    / "frame_id_stream.sse"
)

RUN_ID = "2f6c1a94-8d3e-4b57-9c02-7e5a1d8b3f60"

DONE_SENTINEL = b"data: [DONE]\n\n"


def _session() -> _RunSession:
    return _RunSession(
        run_id=RUN_ID,
        user_id=1,
        request_id=None,
        chat_req=ChatRequest(messages=[ChatMessage(role="user", content="hi")]),
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


def _emit() -> tuple[list[int], list[bytes]]:
    session = _session()
    ids: list[int] = []
    frames: list[bytes] = []
    for event in (
        Event(kind="delta", payload={"delta": "Hello"}),
        Event(
            kind="done",
            payload={
                "usage": {
                    "prompt_tokens": 11,
                    "completion_tokens": 3,
                    "total_tokens": 14,
                }
            },
        ),
    ):
        seq, frame = session.encode_event(event)
        assert seq is not None, "replay is enabled by default — frames must carry ids"
        ids.append(seq)
        frames.append(frame)
    return ids, frames


IDS, FRAMES = _emit()
STREAM = b"".join(FRAMES) + DONE_SENTINEL


def test_frame_id_fixture_matches_emitted_bytes():
    if os.environ.get("PYTHON_AGENT_UPDATE_WIRE_FIXTURES"):
        FIXTURE.write_bytes(STREAM)

    assert FIXTURE.exists(), f"missing fixture: {FIXTURE}"
    assert FIXTURE.read_bytes() == STREAM, (
        "frame id fixture drifted from the emitter — regenerate it with "
        "PYTHON_AGENT_UPDATE_WIRE_FIXTURES=1 and update the TS test's "
        "expected literals in the same change"
    )


def test_ids_are_decimal_and_strictly_increasing():
    assert IDS == [1, 2]
    for seq, frame in zip(IDS, FRAMES):
        head = frame.split(b"\n", 1)[0]
        assert head == b"id: %d" % seq
        assert re.fullmatch(rb"id: \d+", head)


def test_replayed_frames_are_byte_identical():
    """The buffer holds the finished bytes, so a replay is the original."""
    session = _session()
    for event in (
        Event(kind="delta", payload={"delta": "Hello"}),
        Event(
            kind="done",
            payload={
                "usage": {
                    "prompt_tokens": 11,
                    "completion_tokens": 3,
                    "total_tokens": 14,
                }
            },
        ),
    ):
        session.encode_event(event)

    assert session.frame_log is not None
    assert session.frame_log.since(0, upto=2) == FRAMES
    assert session.frame_log.watermark() == 2


def test_replay_disabled_emits_no_id_line():
    session = _session()
    session.frame_log = None

    seq, frame = session.encode_event(Event(kind="delta", payload={"delta": "Hello"}))

    assert seq is None
    assert not frame.startswith(b"id:")
