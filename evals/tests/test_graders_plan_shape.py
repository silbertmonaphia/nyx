"""Plan-shape grader: token normalization, trailing-`llm` equivalence, mode matrix."""

from __future__ import annotations

import json
import sys
from pathlib import Path

import pytest

EVALS_DIR = Path(__file__).resolve().parents[1]
if str(EVALS_DIR) not in sys.path:
    sys.path.insert(0, str(EVALS_DIR))

from graders.plan_shape import get_assert, normalize_steps, strip_answer_tail  # noqa: E402

RAG_STEP = {"id": "step_1", "kind": "rag", "query": "秋刀鱼"}
ANSWER_STEP = {"id": "final_answer", "kind": "llm", "prompt": "write the answer"}


def tool_step(name: str, step_id: str = "step_tool") -> dict:
    return {"id": step_id, "kind": "tool", "tool": name, "args": {}}


def transcript(steps, *, answer="好的", status="completed", skip_reason=None) -> str:
    frames = [
        {"event": "run_id", "data": {"run_id": "run-1"}},
        {"event": "plan", "data": {"steps": steps}},
        {"event": "delta", "data": {"delta": answer}},
        {"event": "done", "data": {"usage": {"total_tokens": 12}}},
    ]
    payload = {
        "status": status,
        "skip_reason": skip_reason,
        "turns": [
            {
                "prompt": "把最近的爱吃的东西整理一下",
                "run_id": "run-1",
                "session_id": "sess-1",
                "frames": frames,
                "answer": answer,
                "error": None,
            }
        ],
        "meta": {"model": "Qwen/Qwen3-4B-Instruct-2507-FP8"},
    }
    return json.dumps(payload, ensure_ascii=False)


def context(expect: dict, *, mode: str | None = None, case_id: str = "rag-basic") -> dict:
    vars_ = {"case_id": case_id, "expect": expect, "prompt": "把最近的爱吃的东西整理一下"}
    if mode is not None:
        vars_["mode"] = {"web_search": mode}
    return {"vars": vars_}


@pytest.fixture(autouse=True)
def _clear_mode_env(monkeypatch):
    monkeypatch.delenv("EVAL_WEB_SEARCH", raising=False)


def test_exact_match_passes():
    result = get_assert(transcript([RAG_STEP, ANSWER_STEP]), context({"plan": ["rag", "llm"]}))
    assert result["pass"] is True
    assert result["score"] == 1.0
    assert "expected=[rag]" in result["reason"]
    assert result["metadata"]["observed_tokens"] == ["rag"]


def test_auto_appended_trailing_llm_is_equivalent():
    result = get_assert(
        transcript([RAG_STEP, tool_step("create_feed"), ANSWER_STEP]),
        context({"plan": ["rag", "create_feed"]}),
    )
    assert result["pass"] is True
    assert result["metadata"]["observed_tokens"] == ["rag", "create_feed"]


def test_model_emitted_trailing_llm_matches_expectation_without_tail():
    result = get_assert(
        transcript([RAG_STEP, ANSWER_STEP]),
        context({"plan": ["rag"]}),
    )
    assert result["pass"] is True


def test_extra_mid_sequence_llm_fails():
    result = get_assert(
        transcript([RAG_STEP, ANSWER_STEP, tool_step("create_feed"), ANSWER_STEP]),
        context({"plan": ["rag", "create_feed"]}),
    )
    assert result["pass"] is False
    assert result["metadata"]["observed_tokens"] == ["rag", "llm", "create_feed"]


def test_only_one_trailing_llm_is_normalized_away():
    result = get_assert(
        transcript([RAG_STEP, ANSWER_STEP, ANSWER_STEP]),
        context({"plan": ["rag", "llm"]}),
    )
    assert result["pass"] is False
    assert result["metadata"]["observed_tokens"] == ["rag", "llm"]


def test_wrong_order_fails():
    result = get_assert(
        transcript([tool_step("create_feed"), RAG_STEP, ANSWER_STEP]),
        context({"plan": ["rag", "create_feed", "llm"]}),
    )
    assert result["pass"] is False
    assert "plan mismatch" in result["reason"]


def test_wrong_tool_fails():
    result = get_assert(
        transcript([RAG_STEP, tool_step("update_feed"), ANSWER_STEP]),
        context({"plan": ["rag", "create_feed", "llm"]}),
    )
    assert result["pass"] is False
    assert "update_feed" in result["reason"]


def test_missing_step_fails():
    result = get_assert(
        transcript([RAG_STEP, ANSWER_STEP]),
        context({"plan": ["rag", "create_feed", "llm"]}),
    )
    assert result["pass"] is False
    assert result["metadata"]["observed_tokens"] == ["rag"]


def test_extra_step_fails():
    result = get_assert(
        transcript([RAG_STEP, tool_step("create_feed"), tool_step("delete_feed"), ANSWER_STEP]),
        context({"plan": ["rag", "create_feed", "llm"]}),
    )
    assert result["pass"] is False
    assert result["metadata"]["observed_tokens"] == ["rag", "create_feed", "delete_feed"]


def test_failure_reason_carries_expectation_observed_and_raw_plan():
    result = get_assert(
        transcript([RAG_STEP, ANSWER_STEP]),
        context({"plan": ["rag", "web_search", "llm"]}),
    )
    assert result["pass"] is False
    assert "expected=[rag, web_search]" in result["reason"]
    assert "observed=[rag]" in result["reason"]
    assert "raw_plan=" in result["reason"]
    assert result["metadata"]["raw_plan"]["steps"][0]["kind"] == "rag"


def test_mode_matrix_selects_the_active_mode():
    expect = {"plan": {"on": ["web_search", "llm"], "off": ["llm"]}}
    off = get_assert(transcript([ANSWER_STEP]), context(expect, mode="off"))
    assert off["pass"] is True
    on = get_assert(transcript([ANSWER_STEP]), context(expect, mode="on"))
    assert on["pass"] is False


def test_mode_matrix_falls_back_to_env(monkeypatch):
    expect = {"plan": {"on": ["web_search", "llm"], "off": ["llm"]}}
    monkeypatch.setenv("EVAL_WEB_SEARCH", "off")
    result = get_assert(transcript([ANSWER_STEP]), context(expect))
    assert result["pass"] is True


def test_mode_defaults_to_on_when_env_is_unset():
    expect = {"plan": {"on": ["web_search", "llm"], "off": ["llm"]}}
    result = get_assert(transcript([tool_step("web_search"), ANSWER_STEP]), context(expect))
    assert result["pass"] is True


def test_mode_matrix_without_active_key_is_skipped_not_failed():
    result = get_assert(
        transcript([ANSWER_STEP]),
        context({"plan": {"off": ["llm"]}}, mode="on"),
    )
    assert result["pass"] is True
    assert result["score"] == 0.0
    assert result["reason"].startswith("SKIPPED:")
    assert "web_search=on" in result["reason"]


def test_skipped_transcript_uses_the_skip_convention():
    result = get_assert(
        transcript([ANSWER_STEP], status="skipped", skip_reason="extraction timeout"),
        context({"plan": ["llm"]}),
    )
    assert result["pass"] is True
    assert result["score"] == 0.0
    assert result["reason"] == "SKIPPED: extraction timeout"


def test_no_plan_frame_fails():
    payload = {"status": "completed", "turns": [{"frames": [], "answer": ""}]}
    result = get_assert(json.dumps(payload), context({"plan": ["llm"]}))
    assert result["pass"] is False
    assert "no plan frame" in result["reason"]


def test_empty_plan_does_not_satisfy_an_llm_expectation():
    result = get_assert(transcript([]), context({"plan": ["llm"]}))
    assert result["pass"] is False
    assert "carries no steps" in result["reason"]


def test_unparseable_output_fails_closed():
    result = get_assert("not json at all", context({"plan": ["llm"]}))
    assert result["pass"] is False
    assert "unusable transcript" in result["reason"]


def test_only_the_last_turns_plan_is_graded():
    payload = {
        "status": "completed",
        "turns": [
            {
                "prompt": "seed",
                "frames": [{"event": "plan", "data": {"steps": [RAG_STEP, ANSWER_STEP]}}],
                "answer": "ok",
            },
            {
                "prompt": "recall",
                "frames": [{"event": "plan", "data": {"steps": [ANSWER_STEP]}}],
                "answer": "秋刀鱼",
            },
        ],
    }
    result = get_assert(json.dumps(payload, ensure_ascii=False), context({"plan": ["llm"]}))
    assert result["pass"] is True


def test_step_normalization_helpers():
    assert normalize_steps([{"kind": "tool", "tool": ""}]) == ["<missing-tool-name>"]
    assert normalize_steps([{"kind": "search"}]) == ["<unknown-kind:search>"]
    assert normalize_steps(["nonsense"]) == ["<malformed-step>"]
    assert strip_answer_tail(["rag", "llm"]) == ["rag"]
    assert strip_answer_tail([]) == []
