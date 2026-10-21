"""Execution-effect grader: the write must execute, not just appear in the plan.

The pre-edit baseline is the motivating transcript: `update_feed` fails
schema validation, the retry substitutes `list_feeds`, and the answer still
claims success. The plan-shape and answer graders both pass that sample;
this grader must not.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

import pytest

EVALS_DIR = Path(__file__).resolve().parents[1]
PROVIDERS_DIR = EVALS_DIR / "providers"
for module_dir in (EVALS_DIR, PROVIDERS_DIR):
    if str(module_dir) not in sys.path:
        sys.path.insert(0, str(module_dir))

import agent_run  # noqa: E402
from graders.tool_effect import get_assert  # noqa: E402

FIXTURES_DIR = Path(__file__).resolve().parent / "fixtures"


def rag_step(step_id: str) -> dict:
    return {"id": step_id, "kind": "rag", "tool": None, "query": "query"}


def llm_step(step_id: str) -> dict:
    return {"id": step_id, "kind": "llm", "tool": None, "prompt": "answer"}


def tool_step(step_id: str, tool: str) -> dict:
    return {"id": step_id, "kind": "tool", "tool": tool, "args": None}


def plan_frame(steps: list[dict]) -> dict:
    return {"event": "plan", "data": {"steps": steps}}


def call_frame(
    step_id: str,
    name: str,
    *,
    call_id: str | None = "call-1",
    args: dict | None = None,
) -> dict:
    data = {"step_id": step_id, "name": name, "args": args or {}}
    if call_id is not None:
        data["call_id"] = call_id
    return {"event": "tool_call", "data": data}


def result_frame(
    step_id: str,
    *,
    call_id: str | None = "call-1",
    ok: bool = True,
    result: dict | None = None,
    error: str | None = None,
) -> dict:
    data = {"step_id": step_id, "ok": ok, "result": result or {}, "error": error}
    if call_id is not None:
        data["call_id"] = call_id
    return {"event": "tool_result", "data": data}


def delta_frame(text: str) -> dict:
    return {"event": "delta", "data": {"delta": text}}


def turn(frames: list[dict], answer: str = "") -> dict:
    return {
        "prompt": "write something",
        "run_id": "run-1",
        "session_id": "run-1",
        "frames": frames,
        "answer": answer,
        "error": None,
    }


def transcript(*turns: dict, meta: dict | None = None) -> dict:
    return {
        "status": "completed",
        "skip_reason": None,
        "turns": list(turns),
        "meta": meta or {},
    }


def context(spec: dict, **expect_extra) -> dict:
    return {"vars": {"case_id": "write-case", "expect": {"tool_effect": spec, **expect_extra}}}


def mcp_result(structured: dict | None = None, content: list[dict] | None = None) -> dict:
    payload: dict = {}
    if structured is not None:
        payload["structuredContent"] = structured
    if content is not None:
        payload["content"] = content
    return payload


CREATE = {
    "tool": "create_feed",
    "effect": "created",
    "description_any_of": ["拉面", "咖喱"],
}
UPDATE = {"tool": "update_feed", "effect": "updated"}
DELETE = {"tool": "delete_feed", "effect": "deleted"}

CREATE_PLAN = [rag_step("step_1"), tool_step("step_2", "create_feed"), llm_step("final")]
UPDATE_PLAN = [rag_step("step_1"), tool_step("step_2", "update_feed"), llm_step("final")]
DELETE_PLAN = [rag_step("step_1"), tool_step("step_2", "delete_feed"), llm_step("final")]


def test_created_passes_from_structured_content() -> None:
    feed = {"id": 284, "title": "总结", "description": "拉面与咖喱的季度回顾"}
    frames = [
        plan_frame(CREATE_PLAN),
        call_frame("step_2", "create_feed"),
        result_frame("step_2", result=mcp_result(structured=feed)),
        delta_frame("已创建"),
    ]
    verdict = get_assert(transcript(turn(frames, "已创建")), context(CREATE))
    assert verdict["pass"] is True, verdict["reason"]
    assert verdict["score"] == 1.0
    assert verdict["metadata"]["content_source"] == "structuredContent"
    assert verdict["metadata"]["feed"]["id"] == 284
    assert verdict["metadata"]["write_executed"] is True


def test_created_passes_from_content_text_json() -> None:
    feed = {"id": 7, "title": "t", "description": "咖喱饭笔记"}
    frames = [
        plan_frame(CREATE_PLAN),
        call_frame("step_2", "create_feed"),
        result_frame(
            "step_2",
            result=mcp_result(
                content=[{"type": "text", "text": json.dumps(feed, ensure_ascii=False)}]
            ),
        ),
        delta_frame("ok"),
    ]
    verdict = get_assert(transcript(turn(frames, "ok")), context(CREATE))
    assert verdict["pass"] is True, verdict["reason"]
    assert verdict["metadata"]["content_source"] == "content_text"


def test_created_passes_from_snapshot_when_payload_has_no_feed() -> None:
    frames = [
        plan_frame(CREATE_PLAN),
        call_frame("step_2", "create_feed"),
        result_frame("step_2", result=mcp_result()),
    ]
    meta = {
        "feeds_before": [{"id": 1, "title": "old", "description": "拉面"}],
        "feeds_after": [
            {"id": 1, "title": "old", "description": "拉面"},
            {"id": 2, "title": "new", "description": "咖喱"},
        ],
    }
    verdict = get_assert(transcript(turn(frames), meta=meta), context(CREATE))
    assert verdict["pass"] is True, verdict["reason"]
    assert verdict["metadata"]["content_source"] == "snapshot"
    assert verdict["metadata"]["feed"]["id"] == 2


def test_created_snapshot_disambiguates_new_feeds_by_keyword() -> None:
    frames = [
        plan_frame(CREATE_PLAN),
        call_frame("step_2", "create_feed"),
        result_frame("step_2", result=mcp_result()),
    ]
    meta = {
        "feeds_before": [],
        "feeds_after": [
            {"id": 1, "title": "other", "description": "无关内容"},
            {"id": 2, "title": "new", "description": "周末做咖喱"},
        ],
    }
    verdict = get_assert(transcript(turn(frames), meta=meta), context(CREATE))
    assert verdict["pass"] is True, verdict["reason"]
    assert verdict["metadata"]["feed"]["id"] == 2


def test_created_snapshot_with_ambiguous_new_feeds_fails() -> None:
    frames = [
        plan_frame(CREATE_PLAN),
        call_frame("step_2", "create_feed"),
        result_frame("step_2", result=mcp_result()),
    ]
    meta = {
        "feeds_before": [],
        "feeds_after": [
            {"id": 1, "title": "a", "description": "拉面"},
            {"id": 2, "title": "b", "description": "咖喱"},
        ],
    }
    verdict = get_assert(transcript(turn(frames), meta=meta), context(CREATE))
    assert verdict["pass"] is False
    assert "cannot identify" in verdict["reason"]


def test_created_placeholder_description_fails() -> None:
    feed = {
        "id": 283,
        "title": "最近3个月Feed总结",
        "description": "对用户过去三个月发布的内容进行归纳与分析，提炼核心主题和趋势。",
    }
    frames = [
        plan_frame(CREATE_PLAN),
        call_frame("step_2", "create_feed"),
        result_frame("step_2", result=mcp_result(structured=feed)),
    ]
    verdict = get_assert(transcript(turn(frames)), context(CREATE))
    assert verdict["pass"] is False
    assert "contains none" in verdict["reason"]
    assert verdict["metadata"]["feed"]["id"] == 283


def test_created_without_payload_or_snapshot_fails() -> None:
    frames = [
        plan_frame(CREATE_PLAN),
        call_frame("step_2", "create_feed"),
        result_frame("step_2", result=mcp_result()),
    ]
    verdict = get_assert(transcript(turn(frames)), context(CREATE))
    assert verdict["pass"] is False
    assert "no created feed" in verdict["reason"]


def test_snapshot_error_is_not_fatal_when_the_payload_carries_the_feed() -> None:
    feed = {"id": 1, "title": "t", "description": "拉面"}
    frames = [
        plan_frame(CREATE_PLAN),
        call_frame("step_2", "create_feed"),
        result_frame("step_2", result=mcp_result(structured=feed)),
    ]
    meta = {"feeds_snapshot_error": "before: page 1: boom"}
    verdict = get_assert(transcript(turn(frames), meta=meta), context(CREATE))
    assert verdict["pass"] is True, verdict["reason"]


def test_snapshot_error_is_fatal_when_the_fallback_is_needed() -> None:
    frames = [
        plan_frame(CREATE_PLAN),
        call_frame("step_2", "create_feed"),
        result_frame("step_2", result=mcp_result()),
    ]
    meta = {"feeds_snapshot_error": "before: page 1: boom"}
    verdict = get_assert(transcript(turn(frames), meta=meta), context(CREATE))
    assert verdict["pass"] is False
    assert "snapshot failed" in verdict["reason"]
    assert "boom" in verdict["reason"]


def test_updated_passes_with_a_payload_feed_row() -> None:
    feed = {"id": 9, "title": "happy", "description": "今天很开心"}
    frames = [
        plan_frame(UPDATE_PLAN),
        call_frame("step_2", "update_feed"),
        result_frame("step_2", result=mcp_result(structured=feed)),
    ]
    verdict = get_assert(transcript(turn(frames)), context(UPDATE))
    assert verdict["pass"] is True, verdict["reason"]
    assert verdict["metadata"]["feed"]["id"] == 9


def test_updated_passes_from_a_changed_snapshot_row() -> None:
    frames = [
        plan_frame(UPDATE_PLAN),
        call_frame("step_2", "update_feed"),
        result_frame("step_2", result=mcp_result()),
    ]
    meta = {
        "feeds_before": [{"id": 9, "title": "sad", "description": "很烦", "updated_at": "t1"}],
        "feeds_after": [{"id": 9, "title": "sad", "description": "很开心", "updated_at": "t2"}],
    }
    verdict = get_assert(transcript(turn(frames), meta=meta), context(UPDATE))
    assert verdict["pass"] is True, verdict["reason"]
    assert verdict["metadata"]["content_source"] == "snapshot"


def test_updated_without_a_changed_row_fails() -> None:
    frames = [
        plan_frame(UPDATE_PLAN),
        call_frame("step_2", "update_feed"),
        result_frame("step_2", result=mcp_result()),
    ]
    row = {"id": 9, "title": "sad", "description": "很烦", "updated_at": "t1"}
    verdict = get_assert(
        transcript(turn(frames), meta={"feeds_before": [row], "feeds_after": [row]}),
        context(UPDATE),
    )
    assert verdict["pass"] is False
    assert "no changed feed" in verdict["reason"]


def test_deleted_passes_from_the_deleted_marker() -> None:
    frames = [
        plan_frame(DELETE_PLAN),
        call_frame("step_2", "delete_feed"),
        result_frame("step_2", result=mcp_result(structured={"deleted": True, "id": 9})),
    ]
    verdict = get_assert(transcript(turn(frames)), context(DELETE))
    assert verdict["pass"] is True, verdict["reason"]
    assert verdict["metadata"]["feed"]["id"] == 9


def test_deleted_passes_when_the_target_id_is_absent_from_the_after_snapshot() -> None:
    frames = [
        plan_frame(DELETE_PLAN),
        call_frame("step_2", "delete_feed", args={"id": 9}),
        result_frame("step_2", result=mcp_result()),
    ]
    meta = {
        "feeds_before": [{"id": 9, "title": "a"}, {"id": 10, "title": "b"}],
        "feeds_after": [{"id": 10, "title": "b"}],
    }
    verdict = get_assert(transcript(turn(frames), meta=meta), context(DELETE))
    assert verdict["pass"] is True, verdict["reason"]
    assert verdict["metadata"]["content_source"] == "snapshot"


def test_deleted_fails_when_the_target_id_is_still_present() -> None:
    frames = [
        plan_frame(DELETE_PLAN),
        call_frame("step_2", "delete_feed", args={"id": 9}),
        result_frame("step_2", result=mcp_result()),
    ]
    meta = {
        "feeds_before": [{"id": 9, "title": "a"}],
        "feeds_after": [{"id": 9, "title": "a"}],
    }
    verdict = get_assert(transcript(turn(frames), meta=meta), context(DELETE))
    assert verdict["pass"] is False
    assert "still present" in verdict["reason"]


def test_deleted_fails_when_the_target_id_never_existed() -> None:
    frames = [
        plan_frame(DELETE_PLAN),
        call_frame("step_2", "delete_feed", args={"id": "feed_id_from_list"}),
        result_frame("step_2", result=mcp_result()),
    ]
    meta = {
        "feeds_before": [{"id": 9, "title": "a"}, {"id": 10, "title": "b"}],
        "feeds_after": [{"id": 9, "title": "a"}, {"id": 10, "title": "b"}],
    }
    verdict = get_assert(transcript(turn(frames), meta=meta), context(DELETE))
    assert verdict["pass"] is False
    assert "not present before the delete" in verdict["reason"]


def test_deleted_reads_the_target_id_from_the_successful_retry() -> None:
    frames = [
        plan_frame(DELETE_PLAN),
        call_frame("step_2", "delete_feed", call_id="c1", args={"id": "feed_id"}),
        result_frame("step_2", call_id="c1", ok=False, error="schema"),
        call_frame("step_2", "delete_feed", call_id="c2", args={"id": 9}),
        result_frame("step_2", call_id="c2", result=mcp_result()),
    ]
    meta = {
        "feeds_before": [{"id": 9, "title": "a"}, {"id": 10, "title": "b"}],
        "feeds_after": [{"id": 10, "title": "b"}],
    }
    verdict = get_assert(transcript(turn(frames), meta=meta), context(DELETE))
    assert verdict["pass"] is True, verdict["reason"]
    assert verdict["metadata"]["feed"]["id"] == 9


def test_deleted_false_marker_fails() -> None:
    frames = [
        plan_frame(DELETE_PLAN),
        call_frame("step_2", "delete_feed"),
        result_frame("step_2", result=mcp_result(structured={"deleted": False, "id": 9})),
    ]
    verdict = get_assert(transcript(turn(frames)), context(DELETE))
    assert verdict["pass"] is False
    assert "deleted=False" in verdict["reason"]


def test_deleted_without_marker_or_snapshot_fails() -> None:
    frames = [
        plan_frame(DELETE_PLAN),
        call_frame("step_2", "delete_feed"),
        result_frame("step_2", result=mcp_result()),
    ]
    verdict = get_assert(transcript(turn(frames)), context(DELETE))
    assert verdict["pass"] is False


def test_failed_write_then_retry_substituting_another_tool_fails() -> None:
    frames = [
        plan_frame(UPDATE_PLAN),
        call_frame(
            "step_2",
            "update_feed",
            call_id="call-1",
            args={"id": "feed_id_from_list"},
        ),
        result_frame(
            "step_2",
            call_id="call-1",
            ok=False,
            error="update_feed: input schema validation failed: /id: Got:string Want:[number]",
        ),
        call_frame("step_2", "list_feeds", call_id="call-2", args={"query": "negative"}),
        result_frame("step_2", call_id="call-2", ok=True, result=mcp_result(structured={"data": []})),
        delta_frame("已根据你的请求，将内容更新为积极情绪表达"),
    ]
    spec = dict(UPDATE, success_claims=["已根据你的请求，将", "更新为积极情绪表达"])
    verdict = get_assert(
        transcript(turn(frames, "已根据你的请求，将内容更新为积极情绪表达")),
        context(spec),
    )
    assert verdict["pass"] is False
    assert "substitutes a different tool" in verdict["reason"]
    assert "list_feeds" in verdict["reason"]
    assert verdict["metadata"]["write_executed"] is False
    assert verdict["metadata"]["executed_tools"] == ["update_feed", "list_feeds"]
    assert verdict["metadata"]["answer_claim_hits"] == [
        "已根据你的请求，将",
        "更新为积极情绪表达",
    ]


def test_same_tool_retry_that_succeeds_passes() -> None:
    feed = {"id": 9, "title": "happy", "description": "开心"}
    frames = [
        plan_frame(UPDATE_PLAN),
        call_frame("step_2", "update_feed", call_id="call-1", args={"feed_id": "x"}),
        result_frame("step_2", call_id="call-1", ok=False, error="Missing:[id]"),
        call_frame("step_2", "update_feed", call_id="call-2", args={"id": 9}),
        result_frame("step_2", call_id="call-2", result=mcp_result(structured=feed)),
        delta_frame("已根据你的请求，将内容更新为积极情绪表达"),
    ]
    spec = dict(UPDATE, success_claims=["已根据你的请求，将"])
    verdict = get_assert(transcript(turn(frames, "已根据你的请求，将内容更新")), context(spec))
    assert verdict["pass"] is True, verdict["reason"]
    assert verdict["metadata"]["write_executed"] is True


def test_missing_tool_result_fails() -> None:
    frames = [
        plan_frame(CREATE_PLAN),
        call_frame("step_2", "create_feed"),
        delta_frame("created"),
    ]
    verdict = get_assert(transcript(turn(frames, "created")), context(CREATE))
    assert verdict["pass"] is False
    assert "no correlated tool_result" in verdict["reason"]


def test_missing_call_id_fails() -> None:
    frames = [
        plan_frame(CREATE_PLAN),
        call_frame("step_2", "create_feed", call_id=None),
        result_frame("step_2", call_id=None),
    ]
    verdict = get_assert(transcript(turn(frames)), context(CREATE))
    assert verdict["pass"] is False
    assert "no call_id" in verdict["reason"]


def test_ok_false_only_fails_and_reports_the_error() -> None:
    frames = [
        plan_frame(CREATE_PLAN),
        call_frame("step_2", "create_feed"),
        result_frame("step_2", ok=False, error="create_feed: boom"),
    ]
    verdict = get_assert(transcript(turn(frames)), context(CREATE))
    assert verdict["pass"] is False
    assert "ok is true" in verdict["reason"]
    assert "boom" in verdict["reason"]


def test_no_tool_call_under_the_expected_step_fails() -> None:
    frames = [plan_frame(CREATE_PLAN), delta_frame("done")]
    verdict = get_assert(transcript(turn(frames)), context(CREATE))
    assert verdict["pass"] is False
    assert "no tool_call" in verdict["reason"]


def test_plan_without_the_write_step_fails() -> None:
    frames = [plan_frame([rag_step("step_1"), llm_step("final")])]
    verdict = get_assert(transcript(turn(frames)), context(CREATE))
    assert verdict["pass"] is False
    assert "names no 'create_feed'" in verdict["reason"]


def test_multiple_write_steps_need_step_index() -> None:
    steps = [
        tool_step("step_1", "create_feed"),
        llm_step("step_2"),
        tool_step("step_3", "create_feed"),
    ]
    verdict = get_assert(transcript(turn([plan_frame(steps)])), context(CREATE))
    assert verdict["pass"] is False
    assert "pin" in verdict["reason"]


def test_step_index_pins_one_of_multiple_write_steps() -> None:
    steps = [
        tool_step("step_1", "create_feed"),
        llm_step("step_2"),
        tool_step("step_3", "create_feed"),
    ]
    frames = [
        plan_frame(steps),
        call_frame("step_3", "create_feed"),
        result_frame(
            "step_3",
            result=mcp_result(structured={"id": 3, "title": "t", "description": "拉面"}),
        ),
    ]
    spec = dict(CREATE, step_index=2)
    verdict = get_assert(transcript(turn(frames)), context(spec))
    assert verdict["pass"] is True, verdict["reason"]
    assert verdict["metadata"]["step_index"] == 2
    assert verdict["metadata"]["step_id"] == "step_3"


def test_step_index_pointing_at_another_step_fails() -> None:
    spec = dict(CREATE, step_index=0)
    verdict = get_assert(
        transcript(turn([plan_frame(CREATE_PLAN)])), context(spec)
    )
    assert verdict["pass"] is False
    assert "not tool 'create_feed'" in verdict["reason"]


def test_step_index_out_of_range_fails() -> None:
    spec = dict(CREATE, step_index=9)
    verdict = get_assert(
        transcript(turn([plan_frame(CREATE_PLAN)])), context(spec)
    )
    assert verdict["pass"] is False
    assert "out of range" in verdict["reason"]


def test_success_claim_without_a_write_fails() -> None:
    spec = dict(DELETE, success_claims=["已从您的Feed列表中永久移除"])
    frames = [
        plan_frame(DELETE_PLAN),
        call_frame("step_2", "delete_feed"),
        result_frame("step_2", ok=False, error="Missing:[id]"),
        delta_frame("已从您的Feed列表中永久移除"),
    ]
    verdict = get_assert(transcript(turn(frames, "已从您的Feed列表中永久移除")), context(spec))
    assert verdict["pass"] is False
    assert "claims success" in verdict["reason"]
    assert verdict["metadata"]["answer_claim_hits"] == ["已从您的Feed列表中永久移除"]


def test_success_claim_with_an_executed_write_is_permitted() -> None:
    spec = dict(DELETE, success_claims=["已从您的Feed列表中永久移除"])
    frames = [
        plan_frame(DELETE_PLAN),
        call_frame("step_2", "delete_feed"),
        result_frame("step_2", result=mcp_result(structured={"deleted": True, "id": 9})),
        delta_frame("已从您的Feed列表中永久移除"),
    ]
    verdict = get_assert(transcript(turn(frames, "已从您的Feed列表中永久移除")), context(spec))
    assert verdict["pass"] is True, verdict["reason"]
    assert verdict["metadata"]["write_executed"] is True


def test_only_the_last_turns_plan_is_graded() -> None:
    first = turn(
        [
            plan_frame(CREATE_PLAN),
            call_frame("step_2", "create_feed"),
            result_frame("step_2", ok=False, error="boom"),
        ]
    )
    second_frames = [
        plan_frame(CREATE_PLAN),
        call_frame("step_2", "create_feed"),
        result_frame(
            "step_2",
            result=mcp_result(structured={"id": 5, "title": "t", "description": "咖喱"}),
        ),
    ]
    verdict = get_assert(
        transcript(first, turn(second_frames)), context(CREATE)
    )
    assert verdict["pass"] is True, verdict["reason"]
    assert verdict["metadata"]["executed_tools"] == ["create_feed"]


def test_last_plan_frame_in_the_turn_wins() -> None:
    frames = [
        plan_frame([rag_step("step_1"), llm_step("final")]),
        plan_frame(CREATE_PLAN),
        call_frame("step_2", "create_feed"),
        result_frame(
            "step_2",
            result=mcp_result(structured={"id": 5, "title": "t", "description": "拉面"}),
        ),
    ]
    verdict = get_assert(transcript(turn(frames)), context(CREATE))
    assert verdict["pass"] is True, verdict["reason"]


def test_reason_never_emits_the_check_rate_observed_token() -> None:
    verdict = get_assert(transcript(turn([])), context(CREATE))
    assert "observed=" not in verdict["reason"]


@pytest.mark.parametrize(
    "spec, fragment",
    [
        (None, "not a mapping"),
        ({"tool": "web_search", "effect": "created"}, "not one of"),
        ({"tool": "create_feed", "effect": "updated"}, "does not match"),
        ({"tool": "create_feed", "effect": "created"}, "required for a created effect"),
        (
            {"tool": "create_feed", "effect": "created", "description_any_of": []},
            "non-empty list",
        ),
        (
            {
                "tool": "delete_feed",
                "effect": "deleted",
                "description_any_of": ["x"],
            },
            "does not apply",
        ),
        ({"tool": "create_feed", "effect": "created", "step_index": -1}, "non-negative"),
        ({"tool": "create_feed", "effect": "created", "step_index": True}, "non-negative"),
        (
            {"tool": "create_feed", "effect": "created", "success_claims": "x"},
            "list of strings",
        ),
        (
            {"tool": "create_feed", "effect": "created", "extra": 1},
            "unknown key",
        ),
    ],
)
def test_malformed_specs_fail_loudly(spec, fragment) -> None:
    expect = {"tool_effect": spec}
    verdict = get_assert(
        transcript(turn([plan_frame(CREATE_PLAN)])),
        {"vars": {"expect": expect}},
    )
    assert verdict["pass"] is False
    assert fragment in verdict["reason"]


UPDATE_CLAIMS = ["已根据你的请求，将", "更新为积极情绪表达"]
DELETE_CLAIMS = ["已根据您的要求，删除了", "已从您的Feed列表中永久移除"]

UPDATE_CORPUS_SPEC = {
    "tool": "update_feed",
    "effect": "updated",
    "success_claims": UPDATE_CLAIMS,
}
DELETE_CORPUS_SPEC = {
    "tool": "delete_feed",
    "effect": "deleted",
    "success_claims": DELETE_CLAIMS,
}

BASELINE_UPDATE_ANSWER = (
    "已根据你的请求，将你近3个月中关于负面情绪的Feed内容"
    "（如压力、焦虑、烦躁等）更新为积极情绪表达，以呈现更阳光、正面的形象。"
)
BASELINE_DELETE_ANSWER = (
    "已根据您的要求，删除了您近3个月所有关于负面情绪的Feed内容。  \n\n"
    "具体已移除的条目包括：  \n"
    "- “这周加班到很晚，好烦，压力大到睡不着，感觉自己快撑不住了。”  \n"
    "- “项目又延期了，心里很焦虑，总觉得要出问题，做什么都提不起劲。”  \n\n"
    "这些内容已从您的Feed列表中永久移除，不会对外显示。"
)


def fixture_frames(name: str) -> list:
    raw = (FIXTURES_DIR / name).read_text(encoding="utf-8")
    blocks = agent_run.iter_sse_blocks(raw.splitlines())
    return [
        frame
        for frame in (agent_run.parse_sse_block(block) for block in blocks)
        if frame is not None
    ]


def fixture_transcript(name: str, answer: str | None = None) -> dict:
    frames = fixture_frames(name)
    if answer is None:
        answer = agent_run.assemble_answer(frames)
    return transcript(turn([frame.as_dict() for frame in frames], answer=answer))


def test_update_fixture_fails_on_the_retry_that_substitutes_list_feeds() -> None:
    verdict = get_assert(
        fixture_transcript("sse_rag_update_feed.sse"), context(UPDATE_CORPUS_SPEC)
    )
    assert verdict["pass"] is False
    metadata = verdict["metadata"]
    assert metadata["executed_tools"] == ["update_feed", "list_feeds"]
    assert metadata["write_executed"] is False
    assert len(metadata["call_ids"]) == 1
    assert "substitutes a different tool" in verdict["reason"]
    assert "input schema validation failed" in verdict["reason"]
    assert "claims success" not in verdict["reason"]
    assert metadata["answer_claim_hits"] == []


def test_delete_fixture_fails_after_two_rejected_ids_and_a_substituted_tool() -> None:
    verdict = get_assert(
        fixture_transcript("sse_rag_delete_feed.sse"), context(DELETE_CORPUS_SPEC)
    )
    assert verdict["pass"] is False
    metadata = verdict["metadata"]
    assert metadata["step_id"] == "delete_feeds"
    assert metadata["executed_tools"] == ["delete_feed", "delete_feed", "list_feeds"]
    assert metadata["write_executed"] is False
    assert len(metadata["call_ids"]) == 2
    assert "substitutes a different tool" in verdict["reason"]
    assert "input schema validation failed" in verdict["reason"]
    assert "mcp: resource not found" in verdict["reason"]
    assert "claims success" not in verdict["reason"]


def test_updated_baseline_claim_trips_the_conditional_gate() -> None:
    verdict = get_assert(
        fixture_transcript("sse_rag_update_feed.sse", answer=BASELINE_UPDATE_ANSWER),
        context(UPDATE_CORPUS_SPEC),
    )
    assert verdict["pass"] is False
    assert verdict["metadata"]["answer_claim_hits"] == UPDATE_CLAIMS
    assert "claims success" in verdict["reason"]


def test_deleted_baseline_claim_trips_the_conditional_gate() -> None:
    verdict = get_assert(
        fixture_transcript("sse_rag_delete_feed.sse", answer=BASELINE_DELETE_ANSWER),
        context(DELETE_CORPUS_SPEC),
    )
    assert verdict["pass"] is False
    assert verdict["metadata"]["answer_claim_hits"] == DELETE_CLAIMS
    assert "claims success" in verdict["reason"]


def test_success_claims_fire_only_when_the_write_did_not_execute() -> None:
    frames = [
        plan_frame(UPDATE_PLAN),
        call_frame("step_2", "update_feed", args={"id": 9}),
        result_frame(
            "step_2",
            result=mcp_result(
                structured={"id": 9, "title": "t", "description": "开心"},
            ),
        ),
    ]
    verdict = get_assert(
        transcript(turn(frames, answer=BASELINE_UPDATE_ANSWER)),
        context(UPDATE_CORPUS_SPEC),
    )
    assert verdict["pass"] is True, verdict["reason"]
    assert verdict["metadata"]["write_executed"] is True
    assert verdict["metadata"]["answer_claim_hits"] == UPDATE_CLAIMS
