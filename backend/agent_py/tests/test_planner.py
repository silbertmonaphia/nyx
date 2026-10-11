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
    assert len(plan.steps) == 2
    assert plan.steps[0].id == "a"
    assert plan.steps[1].tool == "list_feeds"


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


def test_parse_rejects_empty_plan():
    raw = '{"steps":[]}'
    plan = _parse_and_validate(raw, max_steps=10)
    assert isinstance(plan, _PlanError)
    assert plan.code == "plan_invalid"


def test_parse_rejects_non_json():
    plan = _parse_and_validate("not json", max_steps=10)
    assert isinstance(plan, _PlanError)
    assert plan.code == "plan_invalid"