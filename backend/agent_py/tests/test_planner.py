"""Planner unit tests — validate _parse_and_validate paths without an LLM stub.

These cover the validation paths that are LLM-agnostic. The end-to-end planner
that calls ChatOpenAI is exercised in test_run.py with a vcrpy cassette.
"""

from __future__ import annotations

import pytest

from agent.planner import _parse_and_validate, _PlanError


def test_parse_valid_plan():
    raw = '{"steps":[{"id":"a","kind":"llm","prompt":"hi"},{"id":"b","kind":"tool","tool":"list_feeds","args":{}}]}'
    plan = _parse_and_validate(raw, max_steps=10)
    assert not isinstance(plan, _PlanError)
    # 3 steps: the tool ends the model's plan, so the parser appends the
    # answering llm step (see test_parse_appends_answering_llm_step_to_data_only_plan).
    assert len(plan.steps) == 3
    assert plan.steps[0].id == "a"
    assert plan.steps[1].tool == "list_feeds"
    assert plan.steps[2].kind == "llm"


def test_parse_strips_code_fence():
    raw = '```json\n{"steps":[{"id":"a","kind":"llm","prompt":"hi"}]}\n```'
    plan = _parse_and_validate(raw, max_steps=10)
    assert not isinstance(plan, _PlanError)


def test_parse_rejects_oversized_plan():
    steps = ",".join(f'{{"id":"s{i}","kind":"llm","prompt":"hi"}}' for i in range(11))
    raw = '{"steps":[' + steps + ']}'
    plan = _parse_and_validate(raw, max_steps=10)
    assert isinstance(plan, _PlanError)
    assert plan.code == "plan_too_large"


def test_parse_rejects_duplicate_ids():
    raw = '{"steps":[{"id":"a","kind":"llm","prompt":"x"},{"id":"a","kind":"llm","prompt":"y"}]}'
    plan = _parse_and_validate(raw, max_steps=10)
    assert isinstance(plan, _PlanError)
    assert plan.code == "plan_invalid"
    assert "duplicate" in plan.reason


def test_parse_rejects_unknown_kind():
    raw = '{"steps":[{"id":"a","kind":"bogus","prompt":"x"}]}'
    plan = _parse_and_validate(raw, max_steps=10)
    assert isinstance(plan, _PlanError)
    assert plan.code == "plan_invalid"


def test_parse_synthesizes_direct_answer_for_empty_plan():
    """Zero-step plans are converted to a single direct-answer `llm` step.

    Small models deterministically return `{"steps": []}` for greetings
    (Qwen3-4B + "hi"). Rejecting made a greeting surface as
    `plan_invalid` / "Plan is invalid" in the UI. The synthesized step is
    the only sensible reading: no retrieval/tools needed → just answer.
    """
    raw = '{"steps":[]}'
    plan = _parse_and_validate(raw, max_steps=10)
    assert not isinstance(plan, _PlanError)
    assert len(plan.steps) == 1
    assert plan.steps[0].kind == "llm"
    assert plan.steps[0].id
    assert plan.steps[0].prompt


def test_parse_appends_answering_llm_step_to_data_only_plan():
    """A plan ending on a rag/tool step gets a final answering llm step.

    rag/tool emit no user-visible text — the "summarize my feeds" bug was
    a rag-only plan finishing with zero deltas. The prompt asks the model
    to end with an llm step; this pins the deterministic enforcement.
    """
    raw = '{"steps":[{"id":"rag_1","kind":"rag","query":"x"}]}'
    plan = _parse_and_validate(raw, max_steps=10)
    assert not isinstance(plan, _PlanError)
    assert [s.kind.value for s in plan.steps] == ["rag", "llm"]
    assert plan.steps[-1].prompt


def test_parse_leaves_plan_ending_in_llm_unchanged():
    raw = '{"steps":[{"id":"rag_1","kind":"rag","query":"x"},{"id":"llm_1","kind":"llm","prompt":"p"}]}'
    plan = _parse_and_validate(raw, max_steps=10)
    assert not isinstance(plan, _PlanError)
    assert len(plan.steps) == 2


def test_parse_accepts_react_as_answering_step():
    """`react` also produces text deltas — it counts as the answering step."""
    raw = '{"steps":[{"id":"rag_1","kind":"rag","query":"x"},{"id":"react_1","kind":"react","tool":"t","max_iterations":2}]}'
    plan = _parse_and_validate(raw, max_steps=10)
    assert not isinstance(plan, _PlanError)
    assert len(plan.steps) == 2


def test_parse_appended_answering_step_id_avoids_collision():
    """If the model already used the synthesized id, the appended step gets a fresh one."""
    raw = '{"steps":[{"id":"final_answer","kind":"rag","query":"x"}]}'
    plan = _parse_and_validate(raw, max_steps=10)
    assert not isinstance(plan, _PlanError)
    ids = [s.id for s in plan.steps]
    assert len(ids) == len(set(ids)) == 2


def test_parse_rejects_non_json():
    plan = _parse_and_validate("not json", max_steps=10)
    assert isinstance(plan, _PlanError)
    assert plan.code == "plan_invalid"