"""Cross-slice wire contract: captured SSE bytes -> provider parser -> graders.

The provider slice and the grader slice each ship their own suite. This module
is the only place where a real captured SSE stream is parsed by the provider's
own parser, re-rendered through the provider's own transcript renderer, and
then handed to the graders exactly as promptfoo would hand it (a JSON string in
`output`, vars in `context`). A shape drift between the producer and either
consumer fails here even while both slices stay green in isolation.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

import pytest
import yaml

TESTS_DIR = Path(__file__).resolve().parent
EVALS_DIR = TESTS_DIR.parent
FIXTURES_DIR = TESTS_DIR / "fixtures"

if str(EVALS_DIR) not in sys.path:
    sys.path.insert(0, str(EVALS_DIR))
if str(EVALS_DIR / "providers") not in sys.path:
    sys.path.insert(0, str(EVALS_DIR / "providers"))
if str(EVALS_DIR / "cases") not in sys.path:
    sys.path.insert(0, str(EVALS_DIR / "cases"))

import agent_run  # noqa: E402

from graders import answer, plan_shape, security, terminal  # noqa: E402


def _turn_segments(raw: str) -> list[list[str]]:
    """Split a capture on the recorder's `: --- turn N ---` separator."""
    segments: list[list[str]] = [[]]
    for line in raw.splitlines():
        if line.startswith(": --- turn "):
            if any(entry.strip() for entry in segments[-1]):
                segments.append([])
            continue
        segments[-1].append(line)
    return [segment for segment in segments if any(entry.strip() for entry in segment)]


def _fixture_transcript_raw(name: str) -> str:
    """Run a capture through the provider's parser and renderer."""
    raw = (FIXTURES_DIR / name).read_text(encoding="utf-8")
    turns = []
    for segment in _turn_segments(raw):
        frames = [
            frame
            for block in agent_run.iter_sse_blocks(segment)
            if (frame := agent_run.parse_sse_block(block)) is not None
        ]
        run_id = next(
            (frame.data.get("run_id") for frame in frames if frame.event == "run_id"),
            None,
        )
        turns.append(
            {
                "prompt": "",
                "run_id": run_id,
                "session_id": run_id,
                "frames": [frame.as_dict() for frame in frames],
                "answer": agent_run.assemble_answer(frames),
                "error": agent_run.turn_error(frames),
            }
        )
    transcript = {
        "status": "completed",
        "skip_reason": None,
        "turns": turns,
        "meta": {},
    }
    return agent_run.render_transcript(transcript)


def _context(expect: dict, *, case_id: str = "round-trip", mode: str | None = None) -> dict:
    vars_: dict = {"case_id": case_id, "prompt": "round-trip", "expect": expect}
    if mode is not None:
        vars_["mode"] = {"web_search": mode}
    return {"vars": vars_}


PLAN_FIXTURES = [
    ("sse_rag_only.sse", ["rag", "llm"]),
    ("sse_weather_web-on.sse", ["web_search", "llm"]),
    ("sse_combined_multi_step_web-on.sse", ["rag", "web_search", "create_feed", "llm"]),
    ("sse_rag_create_feed.sse", ["rag", "list_feeds", "create_feed", "llm"]),
    ("sse_forced_search_web-on.sse", ["react"]),
    ("sse_direct_answer.sse", ["llm"]),
]


@pytest.mark.parametrize(("fixture", "expected_plan"), PLAN_FIXTURES, ids=[f[0] for f in PLAN_FIXTURES])
def test_recorded_capture_grades_as_expected(fixture: str, expected_plan: list[str]) -> None:
    verdict = plan_shape.get_assert(
        _fixture_transcript_raw(fixture),
        _context({"plan": expected_plan}, mode="on"),
    )
    assert verdict["pass"] is True, verdict["reason"]


@pytest.mark.parametrize("fixture", [fixture for fixture, _ in PLAN_FIXTURES])
def test_recorded_capture_rejects_any_other_expectation(fixture: str) -> None:
    verdict = plan_shape.get_assert(
        _fixture_transcript_raw(fixture),
        _context({"plan": ["rag", "create_feed"]}, mode="on"),
    )
    assert verdict["pass"] is False


def test_trailing_llm_is_equivalent_on_recorded_bytes() -> None:
    verdict = plan_shape.get_assert(
        _fixture_transcript_raw("sse_rag_only.sse"),
        _context({"plan": ["rag"]}, mode="on"),
    )
    assert verdict["pass"] is True


def test_recorded_plan_failure_carries_the_observed_tokens() -> None:
    verdict = plan_shape.get_assert(
        _fixture_transcript_raw("sse_weather_web-on.sse"),
        _context({"plan": ["rag", "web_search", "llm"]}, mode="on"),
    )
    assert verdict["pass"] is False
    assert "web_search" in verdict["reason"]


def test_recorded_terminal_error_codes() -> None:
    refusal = terminal.get_assert(
        _fixture_transcript_raw("sse_refusal_direct_answer.sse"),
        _context({"error_code": "planner_failed"}),
    )
    assert refusal["pass"] is True, refusal["reason"]
    forced = terminal.get_assert(
        _fixture_transcript_raw("sse_forced_search_web-on.sse"),
        _context({"error_code": "empty_answer"}),
    )
    assert forced["pass"] is True, forced["reason"]
    wrong = terminal.get_assert(
        _fixture_transcript_raw("sse_refusal_direct_answer.sse"),
        _context({"error_code": "empty_answer"}),
    )
    assert wrong["pass"] is False


def test_recorded_capture_without_plan_fails_plan_grading_loudly() -> None:
    verdict = plan_shape.get_assert(
        _fixture_transcript_raw("sse_refusal_direct_answer.sse"),
        _context({"plan": ["llm"]}),
    )
    assert verdict["pass"] is False


def test_recorded_dialogue_turn_boundaries() -> None:
    memory = json.loads(_fixture_transcript_raw("sse_memory_dialogue_3turn.sse"))
    assert len(memory["turns"]) == 3
    assert len({turn["run_id"] for turn in memory["turns"]}) == 3
    continuation = json.loads(_fixture_transcript_raw("sse_dialogue_continuation.sse"))
    assert len(continuation["turns"]) == 2
    assert len({turn["run_id"] for turn in continuation["turns"]}) == 1


def test_recorded_memory_recall_grades_on_assembled_deltas() -> None:
    verdict = answer.get_assert(
        _fixture_transcript_raw("sse_memory_dialogue_prefs_landed.sse"),
        _context(
            {
                "turns": [
                    {"index": 2, "answer_contains": ["秋刀鱼"]},
                    {"index": 3, "answer_contains": ["秋刀鱼"]},
                ]
            }
        ),
    )
    assert verdict["pass"] is True, verdict["reason"]


def test_recorded_memory_turn_without_recall_fails() -> None:
    verdict = answer.get_assert(
        _fixture_transcript_raw("sse_memory_dialogue_3turn.sse"),
        _context(
            {
                "turns": [
                    {"index": 2, "answer_contains": ["秋刀鱼"]},
                    {"index": 3, "answer_contains": ["秋刀鱼"]},
                ]
            }
        ),
    )
    assert verdict["pass"] is False


def test_recorded_forbidden_keyword_uses_the_same_deltas() -> None:
    verdict = answer.get_assert(
        _fixture_transcript_raw("sse_memory_dialogue_prefs_landed.sse"),
        _context({"answer_forbidden": ["秋刀鱼"]}),
    )
    assert verdict["pass"] is False


@pytest.mark.parametrize(
    ("fixture", "expect", "expected_pass"),
    [
        ("sse_combined_multi_step_web-on.sse", {"no_destructive_tool_call": True, "allowed_tools": ["create_feed"]}, True),
        ("sse_combined_multi_step_web-on.sse", {"no_destructive_tool_call": True}, False),
        ("sse_rag_create_feed.sse", {"no_destructive_tool_call": True, "allowed_tools": ["create_feed"]}, True),
        ("sse_rag_only.sse", {"no_destructive_tool_call": True}, True),
        ("sse_rag_only.sse", {"no_prompt_leak": True}, True),
        ("sse_memory_dialogue_prefs_landed.sse", {"no_prompt_leak": True}, True),
    ],
)
def test_recorded_capture_security_verdicts(fixture: str, expect: dict, expected_pass: bool) -> None:
    verdict = security.get_assert(_fixture_transcript_raw(fixture), _context(expect))
    assert verdict["pass"] is expected_pass, verdict["reason"]


def test_dialogue_turn_keys_match_the_provider_contract() -> None:
    from lib import corpus as corpus_lib

    assert corpus_lib.DIALOGUE_TURN_KEYS == {"prompt", "new_session", "poll_prefs"}
    turns = agent_run.turn_script(
        {},
        {
            "dialogue": [
                {
                    "prompt": "我最爱吃秋刀鱼",
                    "new_session": True,
                    "poll_prefs": "秋刀鱼",
                }
            ]
        },
    )
    assert set(turns[0]) == {"prompt", "new_session", "poll_prefs"}
    assert turns[0]["poll_prefs"] == "秋刀鱼"


def test_the_old_prefs_key_spelling_is_not_consulted() -> None:
    turns = agent_run.turn_script(
        {},
        {"dialogue": [{"prompt": "seed", "new_session": True, "await_prefs": "秋刀鱼"}]},
    )
    assert turns[0]["poll_prefs"] is None


def test_the_loader_gates_on_the_shared_validator(tmp_path) -> None:
    import loader as case_loader

    bad = tmp_path / "cases.yaml"
    bad.write_text(
        yaml.safe_dump({"cases": [{"id": "bad-case", "line": "main"}]}, allow_unicode=True),
        encoding="utf-8",
    )
    with pytest.raises(Exception) as excinfo:
        case_loader.load_corpus(str(bad))
    assert "bad-case" in str(excinfo.value)
