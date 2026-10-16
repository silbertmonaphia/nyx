"""Answer-content grader: contains / forbidden on the last turn and per turn index."""

from __future__ import annotations

import json
import sys
from pathlib import Path

EVALS_DIR = Path(__file__).resolve().parents[1]
if str(EVALS_DIR) not in sys.path:
    sys.path.insert(0, str(EVALS_DIR))

from graders.answer import get_assert  # noqa: E402


def turn(prompt: str, answer: str) -> dict:
    frames = [{"event": "delta", "data": {"delta": answer}}] if answer else []
    frames.append({"event": "done", "data": {"usage": {"total_tokens": 1}}})
    return {
        "prompt": prompt,
        "run_id": "run-1",
        "session_id": "sess-1",
        "frames": frames,
        "answer": answer,
        "error": None,
    }


def transcript(*turns: dict, status: str = "completed", skip_reason=None) -> str:
    return json.dumps(
        {
            "status": status,
            "skip_reason": skip_reason,
            "turns": list(turns),
            "meta": {"model": "Qwen/Qwen3-4B-Instruct-2507-FP8"},
        },
        ensure_ascii=False,
    )


def context(expect: dict, case_id: str = "memory-秋刀鱼") -> dict:
    return {"vars": {"case_id": case_id, "expect": expect, "prompt": "我喜欢吃什么"}}


def test_contains_passes_on_substring():
    result = get_assert(
        transcript(turn("我喜欢吃什么", "你最近爱吃秋刀鱼，可以试试盐烤。")),
        context({"answer_contains": ["秋刀鱼"]}),
    )
    assert result["pass"] is True


def test_contains_fails_and_reports_the_missing_keyword():
    result = get_assert(
        transcript(turn("我喜欢吃什么", "你之前提到过一些偏好。")),
        context({"answer_contains": ["秋刀鱼"]}),
    )
    assert result["pass"] is False
    assert "秋刀鱼" in result["reason"]


def test_empty_answer_fails_a_contains_assertion():
    result = get_assert(
        transcript(turn("我喜欢吃什么", "")),
        context({"answer_contains": ["秋刀鱼"]}),
    )
    assert result["pass"] is False
    assert "empty" in result["reason"]


def test_forbidden_passes_when_absent():
    result = get_assert(
        transcript(turn("推荐吃的给我", "试试青花鱼。")),
        context({"answer_forbidden": ["秋刀鱼"]}),
    )
    assert result["pass"] is True


def test_forbidden_fails_when_present():
    result = get_assert(
        transcript(turn("推荐吃的给我", "还是秋刀鱼最合适。")),
        context({"answer_forbidden": ["秋刀鱼"]}),
    )
    assert result["pass"] is False
    assert "must NOT contain" in result["reason"]


def test_empty_answer_satisfies_a_forbidden_assertion():
    result = get_assert(
        transcript(turn("推荐吃的给我", "")),
        context({"answer_forbidden": ["秋刀鱼"]}),
    )
    assert result["pass"] is True


def test_contains_and_forbidden_are_combined():
    result = get_assert(
        transcript(turn("我喜欢吃什么", "你喜欢秋刀鱼，也不排斥青花鱼。")),
        context({"answer_contains": ["秋刀鱼"], "answer_forbidden": ["青花鱼"]}),
    )
    assert result["pass"] is False
    assert "青花鱼" in result["reason"]


def test_turns_assert_on_the_indexed_turn():
    transcript_text = transcript(
        turn("我最近爱吃秋刀鱼", "记住了。"),
        turn("我喜欢吃什么", "你爱吃秋刀鱼。"),
        turn("推荐吃的给我", "可以做盐烤秋刀鱼。"),
    )
    result = get_assert(
        transcript_text,
        context({"turns": [{"index": 2, "answer_contains": ["秋刀鱼"]}]}),
    )
    assert result["pass"] is True


def test_turns_index_is_one_based():
    transcript_text = transcript(
        turn("我最近爱吃秋刀鱼", "记住了。"),
        turn("我喜欢吃什么", "你爱吃秋刀鱼。"),
    )
    result = get_assert(
        transcript_text,
        context({"turns": [{"index": 2, "answer_contains": ["秋刀鱼"]}]}),
    )
    assert result["pass"] is True
    wrong = get_assert(
        transcript_text,
        context({"turns": [{"index": 1, "answer_contains": ["秋刀鱼"]}]}),
    )
    assert wrong["pass"] is False


def test_last_turn_keywords_apply_to_the_last_turn_only():
    transcript_text = transcript(
        turn("我最近爱吃秋刀鱼", "你爱吃秋刀鱼。"),
        turn("推荐吃的给我", "试试青花鱼。"),
    )
    result = get_assert(
        transcript_text,
        context({"answer_contains": ["青花鱼"]}),
    )
    assert result["pass"] is True
    earlier_only = get_assert(
        transcript_text,
        context({"answer_contains": ["秋刀鱼"]}),
    )
    assert earlier_only["pass"] is False


def test_turns_index_beyond_the_transcript_fails():
    result = get_assert(
        transcript(turn("我喜欢吃什么", "你爱吃秋刀鱼。")),
        context({"turns": [{"index": 3, "answer_contains": ["秋刀鱼"]}]}),
    )
    assert result["pass"] is False
    assert "cannot grade turn 3" in result["reason"]


def test_malformed_expectation_fails_closed():
    result = get_assert(
        transcript(turn("我喜欢吃什么", "你爱吃秋刀鱼。")),
        context({"turns": [{"index": 0, "answer_contains": ["秋刀鱼"]}]}),
    )
    assert result["pass"] is False
    assert "invalid expectation" in result["reason"]


def test_missing_assertions_fail_closed():
    result = get_assert(transcript(turn("你好", "你好！")), context({}))
    assert result["pass"] is False
    assert "no answer assertions" in result["reason"]


def test_skipped_transcript_uses_the_skip_convention():
    result = get_assert(
        transcript(
            turn("我喜欢吃什么", ""),
            status="skipped",
            skip_reason="preference extraction timeout",
        ),
        context({"answer_contains": ["秋刀鱼"]}),
    )
    assert result["pass"] is True
    assert result["score"] == 0.0
    assert result["reason"] == "SKIPPED: preference extraction timeout"


def test_unparseable_output_fails_closed():
    result = get_assert("<html>not a transcript</html>", context({"answer_contains": ["x"]}))
    assert result["pass"] is False
    assert "unusable transcript" in result["reason"]
