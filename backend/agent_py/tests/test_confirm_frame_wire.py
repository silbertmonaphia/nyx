"""Producer-side pin for the `confirm_request` SSE bytes (review round 1).

The frontend parser test (chatService.test.ts) consumes the checked-in
fixture `frontend/src/features/chat/services/__fixtures__/confirm_request.sse`
through its real parser. THIS test is the producer half of that contract
(design.md D8): it encodes the frame through the real emitter
(`main._sse_encode` + `ConfirmRequestEvent`) and asserts the fixture is
byte-identical — so a producer-side rename, a type change, or a
serialization-order change fails HERE, instead of silently degrading the
UI to "no card for 300 s".

Regenerate the fixture after a deliberate wire change (from backend/agent_py):
    PYTHON_AGENT_UPDATE_WIRE_FIXTURES=1 \
        .venv/bin/python -m pytest tests/test_confirm_frame_wire.py -q
…and update the TS test's expected literals in the same change.
"""

from __future__ import annotations

import os
from pathlib import Path

from agent.main import _sse_encode
from agent.types import ConfirmRequestEvent, Event

FIXTURE = (
    Path(__file__).resolve().parents[3]
    / "frontend"
    / "src"
    / "features"
    / "chat"
    / "services"
    / "__fixtures__"
    / "confirm_request.sse"
)

# Canonical field values shared with the frontend test's expected object —
# change them only by regenerating the fixture and moving the TS literals
# together.
FRAME = _sse_encode(
    Event(
        kind="confirm_request",
        payload=ConfirmRequestEvent(
            run_id="2f6c1a94-8d3e-4b57-9c02-7e5a1d8b3f60",
            step_id="step_2",
            tool="create_feed",
            args={
                "title": "Rust Blog",
                "url": "https://blog.rust-lang.org/feed.xml",
            },
            approval_id="ap_9c4d2f80b1e34a7db6f5c3a2e10d7b45",
            expires_at=1791979500.0,
        ).model_dump(),
    )
)


def test_confirm_request_fixture_matches_emitted_bytes():
    if os.environ.get("PYTHON_AGENT_UPDATE_WIRE_FIXTURES"):
        FIXTURE.write_bytes(FRAME)

    assert FIXTURE.exists(), f"missing fixture: {FIXTURE}"
    assert FIXTURE.read_bytes() == FRAME, (
        "confirm_request fixture drifted from the emitter — regenerate it "
        "with PYTHON_AGENT_UPDATE_WIRE_FIXTURES=1 and update the TS test's "
        "expected literals in the same change"
    )
