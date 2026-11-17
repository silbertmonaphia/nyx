import json
from pathlib import Path

from agent.executor import _idempotency_key

FIXTURE = Path(__file__).parent / "fixtures" / "tool_call_idempotency.json"


def test_agent_payload_matches_shared_wire_fixture():
    payload = json.loads(FIXTURE.read_text())
    assert set(payload) == {"title", "idempotency_key"}
    expected = _idempotency_key(
        "run-wire-1",
        "turn-wire-1",
        "step-wire-1",
        "create_feed",
        {"title": payload["title"]},
    )
    assert payload["idempotency_key"] == expected
