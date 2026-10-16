"""Terminal-frame grader: the run must end with the declared error code."""

from __future__ import annotations

import json
import sys
from pathlib import Path

EVALS_DIR = Path(__file__).resolve().parents[1]
if str(EVALS_DIR) not in sys.path:
    sys.path.insert(0, str(EVALS_DIR))

from graders.terminal import get_assert  # noqa: E402


def error_frame(code: str | None, detail: str = "the run hit a guard") -> dict:
    return {"event": "error", "data": {"error": detail, "code": code, "request_id": "req-1"}}


DONE_FRAME = {"event": "done", "data": {"usage": {"total_tokens": 3}}}


def transcript(frames: list[dict], *, status: str = "error", skip_reason=None) -> str:
    return json.dumps(
        {
            "status": status,
            "skip_reason": skip_reason,
            "turns": [
                {
                    "prompt": "随便聊聊",
                    "run_id": "run-1",
                    "session_id": "sess-1",
                    "frames": frames,
                    "answer": "",
                    "error": None,
                }
            ],
            "meta": {},
        },
        ensure_ascii=False,
    )


def context(expect: dict) -> dict:
    return {"vars": {"case_id": "budget-exhausted", "expect": expect, "prompt": "随便聊聊"}}


def test_expected_error_code_passes():
    result = get_assert(
        transcript([error_frame("budget_exceeded"), DONE_FRAME]),
        context({"error_code": "budget_exceeded"}),
    )
    assert result["pass"] is True
    assert "budget_exceeded" in result["reason"]


def test_different_error_code_fails_and_lists_observed():
    result = get_assert(
        transcript([error_frame("plan_too_large")]),
        context({"error_code": "budget_exceeded"}),
    )
    assert result["pass"] is False
    assert "plan_too_large" in result["reason"]
    assert "budget_exceeded" in result["reason"]


def test_done_instead_of_error_fails():
    result = get_assert(
        transcript([DONE_FRAME], status="completed"),
        context({"error_code": "budget_exceeded"}),
    )
    assert result["pass"] is False
    assert "done frame" in result["reason"]


def test_no_terminal_frame_fails():
    result = get_assert(transcript([]), context({"error_code": "budget_exceeded"}))
    assert result["pass"] is False
    assert "no error frame" in result["reason"]


def test_earlier_error_frame_counts():
    frames = [error_frame("budget_unavailable"), error_frame("budget_exceeded")]
    result = get_assert(transcript(frames), context({"error_code": "budget_exceeded"}))
    assert result["pass"] is True


def test_error_frame_without_a_code_fails():
    result = get_assert(
        transcript([error_frame(None)]),
        context({"error_code": "budget_exceeded"}),
    )
    assert result["pass"] is False
    assert "None" in result["reason"]


def test_missing_error_code_in_expect_fails_closed():
    result = get_assert(transcript([error_frame("budget_exceeded")]), context({}))
    assert result["pass"] is False
    assert "expect.error_code is not set" in result["reason"]


def test_skipped_transcript_uses_the_skip_convention():
    result = get_assert(
        transcript([], status="skipped", skip_reason="requires budget_stack"),
        context({"error_code": "budget_exceeded"}),
    )
    assert result["pass"] is True
    assert result["score"] == 0.0
    assert result["reason"] == "SKIPPED: requires budget_stack"


def test_unparseable_output_fails_closed():
    result = get_assert("", context({"error_code": "budget_exceeded"}))
    assert result["pass"] is False
    assert "unusable transcript" in result["reason"]
