"""Unit tests for `agent.step_trace.StepTrace` (agent-chat-step-replay 3.1).

Pins the pure fold behind the persisted assistant step trace: every frame
kind it consumes, plan ordering, terminal-status protection on re-emitted
frames, duplicate-frame idempotency, ignored frames, and the empty trace
that keeps `steps` NULL.
"""
from __future__ import annotations

import copy

from agent.step_trace import StepTrace


def test_empty_trace_is_none():
    trace = StepTrace()
    assert trace.to_list() is None


def test_plan_seeds_entries_in_plan_order():
    trace = StepTrace()
    trace.fold(
        "plan",
        {
            "steps": [
                {"id": "step_1", "kind": "rag", "query": "q"},
                {
                    "id": "step_2",
                    "kind": "tool",
                    "tool": "list_feeds",
                    "args": {"limit": 5},
                },
                {"id": "step_3", "kind": "llm", "prompt": "answer"},
            ]
        },
    )

    assert trace.to_list() == [
        {"id": "step_1", "kind": "rag", "status": "running"},
        {
            "id": "step_2",
            "kind": "tool",
            "status": "running",
            "tool_name": "list_feeds",
            "tool_args": {"limit": 5},
        },
        {"id": "step_3", "kind": "llm", "status": "running"},
    ]


def test_reemitted_plan_keeps_terminal_status():
    trace = StepTrace()
    plan = {"steps": [{"id": "step_1", "kind": "llm"}]}
    trace.fold("plan", plan)
    trace.fold("step_end", {"step_id": "step_1", "status": "ok"})
    trace.fold("plan", plan)

    assert trace.to_list() == [{"id": "step_1", "kind": "llm", "status": "ok"}]


def test_step_start_upserts_and_marks_running():
    trace = StepTrace()
    trace.fold("step_start", {"step_id": "step_9", "kind": "tool"})
    assert trace.to_list() == [
        {"id": "step_9", "kind": "tool", "status": "running"}
    ]

    trace.fold("step_start", {"step_id": "step_9", "kind": "tool"})
    assert trace.to_list() == [
        {"id": "step_9", "kind": "tool", "status": "running"}
    ]


def test_step_start_does_not_downgrade_terminal_status():
    trace = StepTrace()
    trace.fold("step_start", {"step_id": "step_1", "kind": "rag"})
    trace.fold("step_end", {"step_id": "step_1", "status": "failed"})
    trace.fold("step_start", {"step_id": "step_1", "kind": "rag"})

    assert trace.to_list() == [
        {"id": "step_1", "kind": "rag", "status": "failed"}
    ]


def test_tool_call_keeps_latest_name_and_args():
    trace = StepTrace()
    trace.fold(
        "tool_call",
        {
            "step_id": "step_1",
            "call_id": "call_1",
            "name": "search",
            "args": {"q": "first"},
        },
    )
    trace.fold(
        "tool_call",
        {
            "step_id": "step_1",
            "call_id": "call_2",
            "name": "search_v2",
            "args": {"q": "latest"},
        },
    )

    assert trace.to_list() == [
        {
            "id": "step_1",
            "kind": "tool",
            "status": "running",
            "tool_name": "search_v2",
            "tool_args": {"q": "latest"},
        }
    ]


def test_tool_result_success_sets_result_and_retry_count():
    trace = StepTrace()
    trace.fold(
        "tool_result",
        {
            "step_id": "step_1",
            "call_id": "call_1",
            "ok": True,
            "result": {"items": [1, 2]},
            "error": None,
            "retry_count": 1,
        },
    )

    assert trace.to_list() == [
        {
            "id": "step_1",
            "kind": "tool",
            "status": "running",
            "tool_result": {"items": [1, 2]},
            "retry_count": 1,
        }
    ]


def test_tool_result_failure_sets_error_and_defaults_retry_count():
    trace = StepTrace()
    trace.fold(
        "tool_result",
        {
            "step_id": "step_1",
            "call_id": "call_1",
            "ok": False,
            "result": "ignored",
            "error": "boom",
        },
    )

    assert trace.to_list() == [
        {
            "id": "step_1",
            "kind": "tool",
            "status": "running",
            "tool_result": "boom",
            "retry_count": 0,
        }
    ]


def test_confirm_request_records_only_tool_identity():
    trace = StepTrace()
    trace.fold(
        "confirm_request",
        {
            "run_id": "run-1",
            "step_id": "step_1",
            "tool": "delete_feed",
            "args": {"id": 7},
            "approval_id": "approval-1",
            "expires_at": 123.0,
        },
    )

    assert trace.to_list() == [
        {
            "id": "step_1",
            "kind": "tool",
            "status": "running",
            "tool_name": "delete_feed",
            "tool_args": {"id": 7},
        }
    ]


def test_step_end_sets_status_and_reason():
    failed = StepTrace()
    failed.fold("step_start", {"step_id": "step_1", "kind": "llm"})
    failed.fold(
        "step_end",
        {"step_id": "step_1", "status": "failed", "reason": "provider error"},
    )
    assert failed.to_list() == [
        {
            "id": "step_1",
            "kind": "llm",
            "status": "failed",
            "reason": "provider error",
        }
    ]

    ok = StepTrace()
    ok.fold("step_start", {"step_id": "step_1", "kind": "llm"})
    ok.fold("step_end", {"step_id": "step_1", "status": "ok", "reason": None})
    assert ok.to_list() == [{"id": "step_1", "kind": "llm", "status": "ok"}]


def test_replayed_frames_are_idempotent_and_fold_into_one_entry():
    frames = [
        ("plan", {"steps": [{"id": "step_1", "kind": "tool", "tool": "list"}]}),
        ("step_start", {"step_id": "step_1", "kind": "tool"}),
        (
            "tool_call",
            {
                "step_id": "step_1",
                "call_id": "call_1",
                "name": "list_feeds",
                "args": {"limit": 5},
            },
        ),
        (
            "tool_result",
            {
                "step_id": "step_1",
                "call_id": "call_1",
                "ok": True,
                "result": {"items": []},
                "retry_count": 1,
            },
        ),
        (
            "confirm_request",
            {
                "step_id": "step_1",
                "tool": "list_feeds",
                "args": {"limit": 5},
            },
        ),
        ("step_end", {"step_id": "step_1", "status": "ok"}),
    ]

    once = StepTrace()
    for kind, payload in frames:
        once.fold(kind, payload)
    expected = once.to_list()
    assert expected == [
        {
            "id": "step_1",
            "kind": "tool",
            "status": "ok",
            "tool_name": "list_feeds",
            "tool_args": {"limit": 5},
            "tool_result": {"items": []},
            "retry_count": 1,
        }
    ]

    twice = StepTrace()
    for kind, payload in frames + frames:
        twice.fold(kind, payload)
    assert twice.to_list() == expected


def test_unknown_kinds_and_malformed_payloads_are_ignored():
    trace = StepTrace()
    for kind in ("delta", "done", "note", "error", "run_id", "terminator", "comment"):
        trace.fold(kind, {"delta": "x", "step_id": "step_1", "status": "ok"})
    trace.fold("step_start", None)
    trace.fold("step_start", {"kind": "llm"})
    trace.fold("plan", {"steps": "not-a-list"})
    trace.fold("plan", {"steps": [{"kind": "llm"}, "not-a-mapping"]})

    assert trace.to_list() is None


def test_late_ok_step_end_clears_stale_reason():
    trace = StepTrace()
    trace.fold("step_start", {"step_id": "step_1", "kind": "llm"})
    trace.fold(
        "step_end",
        {"step_id": "step_1", "status": "failed", "reason": "provider error"},
    )
    trace.fold("step_end", {"step_id": "step_1", "status": "ok"})

    assert trace.to_list() == [{"id": "step_1", "kind": "llm", "status": "ok"}]


def test_fold_does_not_mutate_forwarded_payloads():
    trace = StepTrace()
    plan = {
        "steps": [
            {
                "id": "step_1",
                "kind": "tool",
                "tool": "list_feeds",
                "args": {"limit": 5},
            }
        ]
    }
    call = {
        "step_id": "step_1",
        "call_id": "call_1",
        "name": "list_feeds",
        "args": {"limit": 5},
    }
    plan_before = copy.deepcopy(plan)
    call_before = copy.deepcopy(call)

    trace.fold("plan", plan)
    trace.fold("tool_call", call)

    assert plan == plan_before
    assert call == call_before
