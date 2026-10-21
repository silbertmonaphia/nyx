"""Planner unit tests — `_parse_and_validate` paths and the structured-output + retry loop.

The legacy free-text parser tests (`test_parse_*`) exercise `_parse_and_validate`
without an LLM stub. The end-to-end planner that calls ChatOpenAI is exercised
in `test_run.py`.

The structured-output tests (`test_structured_*`, `test_retry_*`) cover the
provider-structured path added by `openspec/changes/agent-planner-structured`.
They monkeypatch `agent.planner._build_structured_client` to avoid any
network egress, plus a fake OTel tracer that swallows span starts so the
test doesn't require a running OTLP collector. Wire-level behaviour of the
real bound chain is covered by `test_planner_parity.py`.
"""

from __future__ import annotations

import pytest
from langchain_core.exceptions import OutputParserException
from pydantic import ValidationError

from agent.config import Settings
from agent.observability import agent_planner_parse_failures_total
from agent.planner import (
    _AUTO_ANSWER_PROMPT,
    PLANNER_SYSTEM_PROMPT,
    PREVIOUS_ATTEMPT_FAILED_HEADER,
    _append_error_to_messages,
    _emit_legacy_plan,
    _emit_structured_plan,
    _enforce_plan_shape,
    _extract_structured_error,
    _latest_user_message,
    _parse_and_validate,
    _PlanError,
    _state_change_intents,
    plan_node,
)
from agent.state import new_state
from agent.types import Plan, PlanStep, StepKind


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


# ---------------------------------------------------------------------------
# Structured-output path (openspec/changes/agent-planner-structured)
# ---------------------------------------------------------------------------


class _NoopTracer:
    """Minimal OTel tracer stand-in for tests.

    The real `opentelemetry.trace.Tracer` would require a global provider
    set up via `setup_observability`. The planner only calls
    `tracer.start_as_current_span(...)` as a context manager, so any object
    that yields an empty `Span` no-op works.
    """

    def start_as_current_span(self, name, **_attrs):
        from contextlib import nullcontext

        span = type("Span", (), {"set_attribute": lambda self, k, v: None})()
        return nullcontext(span)


class _ScriptedStructuredClient:
    """Mock for the chain returned by `_build_structured_client(...)`.

    Each `ainvoke` call pops the next action from `_script`:
    `"raise_validation"` raises a synthesized `ValidationError`,
    `"raise_parser"` raises an `OutputParserException`, `"ok"` returns the
    corresponding canned `Plan` from `_successes`.

    Returns a `Plan` instance directly (what the real bound chain's
    parser + validator produce).
    """

    def __init__(self, script=None, successes=None):
        # script entries: "raise_validation" | "raise_parser" | "ok"
        self._script = list(script or [])
        self._successes = list(successes or [])
        self._call_count = 0
        self._captured_messages: list = []

    async def ainvoke(self, messages, config=None):
        self._call_count += 1
        self._captured_messages.append(messages)
        if not self._script:
            raise AssertionError("scripted client called more times than scripted")
        action = self._script.pop(0)
        if action == "raise_validation":
            try:
                Plan.model_validate({"steps": [{"kind": "bogus"}]})
            except ValidationError as exc:
                raise ValidationError.from_exception_data(
                    title="Plan", line_errors=exc.errors()
                ) from exc
        if action == "raise_parser":
            # First positional arg IS `error` in langchain-core 1.6.9 —
            # there is no `.error` attribute; the message lives in
            # args[0] / str(exc).
            raise OutputParserException(
                "steps.0.kind: Input should be 'rag','tool','llm' or 'react'",
                observation="{}",
            )
        if action == "ok":
            return self._successes.pop(0)
        raise AssertionError(f"unknown scripted action: {action}")


@pytest.fixture
def scripted_settings(monkeypatch):
    """Settings pinned to the structured path with one retry budgeted."""
    monkeypatch.setenv("PYTHON_AGENT_LLM_BASE_URL", "http://planner.test/v1")
    monkeypatch.setenv("PYTHON_AGENT_LLM_API_KEY", "test-key")
    monkeypatch.setenv("PYTHON_AGENT_LLM_MODEL", "test-model")
    monkeypatch.setenv("PYTHON_AGENT_PLANNER_RETRY_MAX", "1")
    monkeypatch.setenv("PYTHON_AGENT_PLANNER_STRUCTURED_ENABLED", "true")
    monkeypatch.setenv("PYTHON_AGENT_PLAN_MAX_STEPS", "5")
    return Settings(_env_file=None)  # hermetic: never read the developer's .env


def _counter_value(label: str) -> float:
    return agent_planner_parse_failures_total.labels(retry=label)._value.get()


def test_structured_returns_parsed_plan_first_try(monkeypatch, scripted_settings):
    """Primary path: structured binding returns `Plan` on first try, no retry."""
    canned = Plan(
        steps=[PlanStep(id="step_1", kind=StepKind.llm, prompt="hello")]
    )
    client = _ScriptedStructuredClient(script=["ok"], successes=[canned])
    monkeypatch.setattr(
        "agent.planner._build_structured_client", lambda _s: client
    )

    state = new_state(
        user_id=1,
        run_id="r1",
        model="test-model",
        messages=[{"role": "user", "content": "hi"}],
    )
    plan_or_error = await_call(
        _emit_structured_plan(state, scripted_settings, _NoopTracer())
    )

    assert isinstance(plan_or_error, Plan)
    assert plan_or_error.steps[0].id == "step_1"
    assert client._call_count == 1


def test_retry_injects_validator_error(monkeypatch, scripted_settings):
    """First attempt raises ValidationError; the second prompt carries the error."""
    first_error = ValidationError.from_exception_data(
        title="Plan",
        line_errors=[
            {
                "type": "missing",
                "loc": ("steps", 0, "id"),
                "msg": "Field required",
                "input": {},
            }
        ],
    )
    canned = Plan(steps=[PlanStep(id="s1", kind=StepKind.llm, prompt="x")])
    # Override the mock to raise the real ValidationError on the first call.
    real_validation_error = first_error

    class _RealErrorClient(_ScriptedStructuredClient):
        async def ainvoke(self, messages, config=None):
            self._call_count += 1
            self._captured_messages.append(messages)
            action = self._script.pop(0)
            if action == "raise_validation":
                raise real_validation_error
            return self._successes.pop(0)

    client = _RealErrorClient(script=["raise_validation", "ok"], successes=[canned])
    monkeypatch.setattr("agent.planner._build_structured_client", lambda _s: client)

    state = new_state(
        user_id=1,
        run_id="r1",
        model="test-model",
        messages=[{"role": "user", "content": "hi"}],
    )
    plan_or_error = await_call(
        _emit_structured_plan(state, scripted_settings, _NoopTracer())
    )

    assert isinstance(plan_or_error, Plan)
    assert client._call_count == 2
    # The second prompt's last message MUST contain the validator's error text.
    second_prompt = client._captured_messages[1][-1].content
    assert PREVIOUS_ATTEMPT_FAILED_HEADER in second_prompt
    assert "Field required" in second_prompt


def test_retry_succeeds_after_validation_failure(monkeypatch, scripted_settings):
    """After a transient validation failure, the second attempt's Plan wins."""
    canned = Plan(steps=[PlanStep(id="step_1", kind=StepKind.llm, prompt="recovered")])

    real_error = ValidationError.from_exception_data(
        title="Plan",
        line_errors=[
            {
                "type": "missing",
                "loc": ("steps", 0, "id"),
                "msg": "Field required",
                "input": {},
            }
        ],
    )

    class _RecoveringClient(_ScriptedStructuredClient):
        async def ainvoke(self, messages, config=None):
            self._call_count += 1
            self._captured_messages.append(messages)
            action = self._script.pop(0)
            if action == "raise_validation":
                raise real_error
            return self._successes.pop(0)

    client = _RecoveringClient(script=["raise_validation", "ok"], successes=[canned])
    monkeypatch.setattr("agent.planner._build_structured_client", lambda _s: client)

    state = new_state(
        user_id=1,
        run_id="r1",
        model="test-model",
        messages=[{"role": "user", "content": "hi"}],
    )
    plan_or_error = await_call(_emit_structured_plan(state, scripted_settings, _NoopTracer()))

    assert isinstance(plan_or_error, Plan)
    assert plan_or_error.steps[0].prompt == "recovered"
    assert client._call_count == 2


def test_retry_exhaustion_emits_plan_invalid(monkeypatch, scripted_settings):
    """When every attempt raises ValidationError, the planner emits `plan_invalid`."""

    real_error = ValidationError.from_exception_data(
        title="Plan",
        line_errors=[
            {
                "type": "missing",
                "loc": ("steps", 0, "id"),
                "msg": "Field required",
                "input": {},
            }
        ],
    )

    class _AlwaysFailingClient(_ScriptedStructuredClient):
        async def ainvoke(self, messages, config=None):
            self._call_count += 1
            self._captured_messages.append(messages)
            raise real_error

    client = _AlwaysFailingClient(script=["raise_validation", "raise_validation"])
    monkeypatch.setattr("agent.planner._build_structured_client", lambda _s: client)

    state = new_state(
        user_id=1,
        run_id="r1",
        model="test-model",
        messages=[{"role": "user", "content": "hi"}],
    )
    plan_or_error = await_call(_emit_structured_plan(state, scripted_settings, _NoopTracer()))

    assert isinstance(plan_or_error, _PlanError)
    assert plan_or_error.code == "plan_invalid"
    assert client._call_count == 2  # one initial + one retry (planner_retry_max=1)


def test_retry_cap_zero_is_one_attempt(monkeypatch):
    """PYTHON_AGENT_PLANNER_RETRY_MAX=0 short-circuits to a single attempt."""
    monkeypatch.setenv("PYTHON_AGENT_LLM_BASE_URL", "http://planner.test/v1")
    monkeypatch.setenv("PYTHON_AGENT_LLM_API_KEY", "test-key")
    monkeypatch.setenv("PYTHON_AGENT_LLM_MODEL", "test-model")
    monkeypatch.setenv("PYTHON_AGENT_PLANNER_RETRY_MAX", "0")
    monkeypatch.setenv("PYTHON_AGENT_PLANNER_STRUCTURED_ENABLED", "true")
    settings = Settings(_env_file=None)  # hermetic: never read the developer's .env

    real_error = ValidationError.from_exception_data(
        title="Plan",
        line_errors=[
            {
                "type": "missing",
                "loc": ("steps", 0, "id"),
                "msg": "Field required",
                "input": {},
            }
        ],
    )

    class _AlwaysFailingClient(_ScriptedStructuredClient):
        async def ainvoke(self, messages, config=None):
            self._call_count += 1
            raise real_error

    client = _AlwaysFailingClient(script=["raise_validation"])
    monkeypatch.setattr("agent.planner._build_structured_client", lambda _s: client)

    state = new_state(
        user_id=1,
        run_id="r1",
        model="test-model",
        messages=[{"role": "user", "content": "hi"}],
    )
    plan_or_error = await_call(_emit_structured_plan(state, settings, _NoopTracer()))

    assert isinstance(plan_or_error, _PlanError)
    assert client._call_count == 1


def test_retry_recovers_from_output_parser_exception(monkeypatch, scripted_settings):
    """The OutputParserException path is retryable and recovers on attempt 2."""
    canned = Plan(steps=[PlanStep(id="s1", kind=StepKind.llm, prompt="p")])
    client = _ScriptedStructuredClient(script=["raise_parser", "ok"], successes=[canned])
    monkeypatch.setattr("agent.planner._build_structured_client", lambda _s: client)

    state = new_state(
        user_id=1,
        run_id="r1",
        model="test-model",
        messages=[{"role": "user", "content": "hi"}],
    )
    plan_or_error = await_call(
        _emit_structured_plan(state, scripted_settings, _NoopTracer())
    )

    assert isinstance(plan_or_error, Plan)
    assert client._call_count == 2
    # The retry prompt carries the parser message (not the doc URL).
    retry_prompt = client._captured_messages[1][-1].content
    assert "Input should be" in retry_prompt
    assert "For troubleshooting" not in retry_prompt


def test_retry_max_two_permits_two_retries(monkeypatch):
    monkeypatch.setenv("PYTHON_AGENT_LLM_BASE_URL", "http://planner.test/v1")
    monkeypatch.setenv("PYTHON_AGENT_LLM_API_KEY", "test-key")
    monkeypatch.setenv("PYTHON_AGENT_LLM_MODEL", "test-model")
    monkeypatch.setenv("PYTHON_AGENT_PLANNER_RETRY_MAX", "2")
    monkeypatch.setenv("PYTHON_AGENT_PLANNER_STRUCTURED_ENABLED", "true")
    monkeypatch.setenv("PYTHON_AGENT_PLAN_MAX_STEPS", "5")
    settings = Settings(_env_file=None)  # hermetic: never read the developer's .env

    canned = Plan(steps=[PlanStep(id="s1", kind=StepKind.llm, prompt="p")])
    client = _ScriptedStructuredClient(
        script=["raise_parser", "raise_parser", "ok"], successes=[canned]
    )
    monkeypatch.setattr("agent.planner._build_structured_client", lambda _s: client)

    state = new_state(
        user_id=1,
        run_id="r1",
        model="test-model",
        messages=[{"role": "user", "content": "hi"}],
    )
    plan_or_error = await_call(_emit_structured_plan(state, settings, _NoopTracer()))

    assert isinstance(plan_or_error, Plan)
    assert client._call_count == 3  # initial + two retries


def test_plan_node_emits_error_and_done_on_retry_exhaustion(
    monkeypatch, scripted_settings
):
    """Spec scenario "Persistent Pydantic failure terminates run": the node
    emits `error{code:"plan_invalid"}` followed by `done` (+ `terminator`)
    and never transitions past planning. Safe-error discipline: the detail
    is the static string, not raw exception text.

    The partial-failure note sits between error and done
    (agent-tool-dispatch-retry): plan-level failures are a plan divergence,
    and the note must ride the PLANNER's chunk — `run_agent_stream` slices
    positionally, so a note inserted from the executor node would never
    reach the wire (test-review round 1, F1)."""
    real_error = ValidationError.from_exception_data(
        title="Plan",
        line_errors=[
            {
                "type": "missing",
                "loc": ("steps", 0, "id"),
                "msg": "Field required",
                "input": {},
            }
        ],
    )

    class _AlwaysFailingClient(_ScriptedStructuredClient):
        async def ainvoke(self, messages, config=None):
            self._call_count += 1
            raise real_error

    client = _AlwaysFailingClient(script=["x", "x"])
    monkeypatch.setattr("agent.planner._build_structured_client", lambda _s: client)

    state = new_state(
        user_id=1,
        run_id="r1",
        model="test-model",
        messages=[{"role": "user", "content": "hi"}],
    )
    out = await_call(plan_node(state, scripted_settings, _NoopTracer()))

    kinds = [e.kind for e in out["events_so_far"]]
    assert kinds == ["error", "note", "done", "terminator"]
    err_payload = out["events_so_far"][0].payload
    assert err_payload["code"] == "plan_invalid"
    assert err_payload["error"] == "Plan is invalid"
    note_payload = out["events_so_far"][1].payload
    assert note_payload["kind"] == "partial_failure"
    assert note_payload["failed_steps"] == []
    assert out["terminal_status"] == "failed"
    assert out.get("plan") is None


def test_emit_error_note_gated_on_plan_level_codes():
    """Round 2, NEW-2: the partial-failure note rides only the spec's CLOSED
    trigger set. `planner_failed` (a provider/construction fault — the run
    never had a plan to diverge from) must NOT emit it."""
    from agent.planner import _emit_error

    state = new_state(
        user_id=1, run_id="r1", model="m", messages=[{"role": "user", "content": "hi"}]
    )
    out = _emit_error(state, code="plan_too_large", detail="too many steps")
    assert [e.kind for e in out["events_so_far"]] == ["error", "note", "done", "terminator"]

    state2 = new_state(
        user_id=1, run_id="r2", model="m", messages=[{"role": "user", "content": "hi"}]
    )
    out2 = _emit_error(state2, code="planner_failed", detail="provider down")
    assert [e.kind for e in out2["events_so_far"]] == ["error", "done", "terminator"]


def test_retry_increments_ok_counter(monkeypatch, scripted_settings):
    """`retry="ok"` increments when a transient failure recovers on the next attempt."""
    before = _counter_value("ok")

    canned = Plan(steps=[PlanStep(id="s1", kind=StepKind.llm, prompt="x")])
    real_error = ValidationError.from_exception_data(
        title="Plan",
        line_errors=[
            {
                "type": "missing",
                "loc": ("steps", 0, "id"),
                "msg": "Field required",
                "input": {},
            }
        ],
    )

    class _RecoveringClient(_ScriptedStructuredClient):
        async def ainvoke(self, messages, config=None):
            self._call_count += 1
            action = self._script.pop(0)
            if action == "raise_validation":
                raise real_error
            return self._successes.pop(0)

    client = _RecoveringClient(script=["raise_validation", "ok"], successes=[canned])
    monkeypatch.setattr("agent.planner._build_structured_client", lambda _s: client)

    state = new_state(
        user_id=1,
        run_id="r1",
        model="test-model",
        messages=[{"role": "user", "content": "hi"}],
    )
    await_call(_emit_structured_plan(state, scripted_settings, _NoopTracer()))

    after = _counter_value("ok")
    assert after - before == 1


def test_retry_exhaustion_increments_exhausted_counter(monkeypatch, scripted_settings):
    """`retry="exhausted"` increments when the retry budget is exhausted."""
    before = _counter_value("exhausted")

    real_error = ValidationError.from_exception_data(
        title="Plan",
        line_errors=[
            {
                "type": "missing",
                "loc": ("steps", 0, "id"),
                "msg": "Field required",
                "input": {},
            }
        ],
    )

    class _AlwaysFailingClient(_ScriptedStructuredClient):
        async def ainvoke(self, messages, config=None):
            self._call_count += 1
            raise real_error

    client = _AlwaysFailingClient(script=["raise_validation", "raise_validation"])
    monkeypatch.setattr("agent.planner._build_structured_client", lambda _s: client)

    state = new_state(
        user_id=1,
        run_id="r1",
        model="test-model",
        messages=[{"role": "user", "content": "hi"}],
    )
    await_call(_emit_structured_plan(state, scripted_settings, _NoopTracer()))

    after = _counter_value("exhausted")
    assert after - before == 1


def test_terminal_plan_too_large_preserves_code_and_no_exhausted_tick(
    monkeypatch, scripted_settings
):
    """A terminal oversized plan keeps `plan_too_large` (not `plan_invalid`)
    and does NOT tick the parse-failure counter (spec ties `exhausted` to
    `plan_invalid` terminations)."""
    before_exhausted = _counter_value("exhausted")

    oversized = Plan(
        steps=[PlanStep(id=f"s{i}", kind=StepKind.llm, prompt="x") for i in range(11)]
    )

    class _OversizedClient(_ScriptedStructuredClient):
        async def ainvoke(self, messages, config=None):
            self._call_count += 1
            self._captured_messages.append(messages)
            return oversized

    # scripted_settings pins plan_max_steps=5, retry_max=1 → 2 attempts, both oversized.
    client = _OversizedClient(script=["ok", "ok"], successes=[])
    monkeypatch.setattr("agent.planner._build_structured_client", lambda _s: client)

    state = new_state(
        user_id=1,
        run_id="r1",
        model="test-model",
        messages=[{"role": "user", "content": "hi"}],
    )
    plan_or_error = await_call(
        _emit_structured_plan(state, scripted_settings, _NoopTracer())
    )

    assert isinstance(plan_or_error, _PlanError)
    assert plan_or_error.code == "plan_too_large"
    assert client._call_count == 2  # initial + one retry
    assert _counter_value("exhausted") - before_exhausted == 0


def test_provider_error_returns_planner_failed_no_retry(monkeypatch, scripted_settings):
    """A non-retryable provider fault surfaces as typed `planner_failed`
    (legacy parity) and is NOT retried."""

    class _ProviderErrorClient(_ScriptedStructuredClient):
        async def ainvoke(self, messages, config=None):
            self._call_count += 1
            self._captured_messages.append(messages)
            raise RuntimeError("connection reset by peer")

    client = _ProviderErrorClient(script=["ok", "ok"])
    monkeypatch.setattr("agent.planner._build_structured_client", lambda _s: client)

    state = new_state(
        user_id=1,
        run_id="r1",
        model="test-model",
        messages=[{"role": "user", "content": "hi"}],
    )
    plan_or_error = await_call(
        _emit_structured_plan(state, scripted_settings, _NoopTracer())
    )

    assert isinstance(plan_or_error, _PlanError)
    assert plan_or_error.code == "planner_failed"
    assert client._call_count == 1  # no retry on provider faults


def test_construction_fault_returns_planner_failed(monkeypatch, scripted_settings):
    """A fault while building the bound client (e.g. an incompatible
    langchain-openai, invalid base_url) must surface as the typed
    `planner_failed` error, never escape the graph node."""

    def _raise(_settings):
        raise RuntimeError("client construction exploded")

    monkeypatch.setattr("agent.planner._build_structured_client", _raise)

    state = new_state(
        user_id=1,
        run_id="r1",
        model="test-model",
        messages=[{"role": "user", "content": "hi"}],
    )
    plan_or_error = await_call(
        _emit_structured_plan(state, scripted_settings, _NoopTracer())
    )

    assert isinstance(plan_or_error, _PlanError)
    assert plan_or_error.code == "planner_failed"
    # Wire-safety: `reason` feeds the terminator frame (old clients render
    # it), so provider reprs — which can echo credential fragments — must
    # NOT appear there; the raw text goes to the log/span instead.
    assert plan_or_error.reason == "provider call failed"
    assert "construction exploded" not in plan_or_error.reason


def test_non_plan_return_is_retryable_and_recovers(monkeypatch, scripted_settings):
    """A chain yielding a non-`Plan` (`None`) must be retried with feedback,
    not crash — the graph-node crash class that escaped the first version."""
    canned = Plan(steps=[PlanStep(id="s1", kind=StepKind.llm, prompt="p")])

    class _NoneThenPlanClient(_ScriptedStructuredClient):
        async def ainvoke(self, messages, config=None):
            self._call_count += 1
            self._captured_messages.append(messages)
            if self._call_count == 1:
                return None
            return canned

    client = _NoneThenPlanClient(script=["ok", "ok"])
    monkeypatch.setattr("agent.planner._build_structured_client", lambda _s: client)

    state = new_state(
        user_id=1,
        run_id="r1",
        model="test-model",
        messages=[{"role": "user", "content": "hi"}],
    )
    plan_or_error = await_call(
        _emit_structured_plan(state, scripted_settings, _NoopTracer())
    )

    assert isinstance(plan_or_error, Plan)
    assert client._call_count == 2
    # The retry prompt names the actual problem (NoneType), not a crash.
    assert "NoneType" in client._captured_messages[1][-1].content


def test_non_plan_return_exhaustion_ticks_exhausted(monkeypatch, scripted_settings):
    """Persistent non-`Plan` returns terminate with `plan_invalid` and a
    single `exhausted` tick."""
    before = _counter_value("exhausted")

    class _AlwaysNoneClient(_ScriptedStructuredClient):
        async def ainvoke(self, messages, config=None):
            self._call_count += 1

    client = _AlwaysNoneClient(script=["ok", "ok"])
    monkeypatch.setattr("agent.planner._build_structured_client", lambda _s: client)

    state = new_state(
        user_id=1,
        run_id="r1",
        model="test-model",
        messages=[{"role": "user", "content": "hi"}],
    )
    plan_or_error = await_call(
        _emit_structured_plan(state, scripted_settings, _NoopTracer())
    )

    assert isinstance(plan_or_error, _PlanError)
    assert plan_or_error.code == "plan_invalid"
    assert _counter_value("exhausted") - before == 1


def test_legacy_terminal_plan_invalid_ticks_exhausted(monkeypatch):
    """Spec: `exhausted` counts runs terminating with `plan_invalid` — the
    legacy free-text path included (no retry there, so a parse failure is
    terminal immediately). `plan_too_large` terminations do NOT tick."""
    monkeypatch.setenv("PYTHON_AGENT_LLM_BASE_URL", "http://planner.test/v1")
    monkeypatch.setenv("PYTHON_AGENT_LLM_API_KEY", "test-key")
    monkeypatch.setenv("PYTHON_AGENT_LLM_MODEL", "test-model")
    monkeypatch.setenv("PYTHON_AGENT_PLANNER_STRUCTURED_ENABLED", "false")
    settings = Settings(_env_file=None)  # hermetic: never read the developer's .env

    from langchain_core.messages import AIMessage

    class _GarbageClient:
        def __init__(self, *args, **kwargs):
            pass

        async def ainvoke(self, messages, config=None):
            return AIMessage(content="not json at all")

    monkeypatch.setattr("agent.planner.ChatOpenAI", _GarbageClient)

    state = new_state(
        user_id=1,
        run_id="r1",
        model="test-model",
        messages=[{"role": "user", "content": "hi"}],
    )
    before = _counter_value("exhausted")
    plan_or_error = await_call(_emit_legacy_plan(state, settings, _NoopTracer()))
    assert isinstance(plan_or_error, _PlanError)
    assert plan_or_error.code == "plan_invalid"
    assert _counter_value("exhausted") - before == 1

    # plan_too_large terminates without ticking (spec ties exhausted to plan_invalid).
    class _OversizedClient:
        def __init__(self, *args, **kwargs):
            pass

        async def ainvoke(self, messages, config=None):
            steps = ",".join(
                f'{{"id":"s{i}","kind":"llm","prompt":"x"}}' for i in range(11)
            )
            return AIMessage(content='{"steps":[' + steps + "]}")

    monkeypatch.setattr("agent.planner.ChatOpenAI", _OversizedClient)

    before = _counter_value("exhausted")
    plan_or_error = await_call(_emit_legacy_plan(state, settings, _NoopTracer()))
    assert isinstance(plan_or_error, _PlanError)
    assert plan_or_error.code == "plan_too_large"
    assert _counter_value("exhausted") - before == 0


def test_enforce_plan_shape_appends_answering_step():
    """Shared enrichment appends an answering llm step to data-only plans."""
    plan = Plan(steps=[PlanStep(id="rag_1", kind=StepKind.rag, query="x")])
    out = _enforce_plan_shape(plan, max_steps=10)
    assert isinstance(out, Plan)
    assert [s.kind.value for s in out.steps] == ["rag", "llm"]


def test_enforce_plan_shape_empty_plan_synthesizes_step():
    plan = Plan(steps=[])
    out = _enforce_plan_shape(plan, max_steps=10)
    assert isinstance(out, Plan)
    assert len(out.steps) == 1
    assert out.steps[0].kind == StepKind.llm


def test_enforce_plan_shape_rejects_oversized():
    plan = Plan(
        steps=[
            PlanStep(id=f"s{i}", kind=StepKind.llm, prompt="x") for i in range(11)
        ]
    )
    out = _enforce_plan_shape(plan, max_steps=10)
    assert isinstance(out, _PlanError)
    assert out.code == "plan_too_large"


def test_enforce_plan_shape_rejects_duplicate_ids():
    plan = Plan(
        steps=[
            PlanStep(id="dup", kind=StepKind.llm, prompt="a"),
            PlanStep(id="dup", kind=StepKind.llm, prompt="b"),
        ]
    )
    out = _enforce_plan_shape(plan, max_steps=10)
    assert isinstance(out, _PlanError)
    assert out.code == "plan_invalid"


def test_structured_disabled_routes_through_legacy(monkeypatch):
    """When `planner_structured_enabled=False`, the legacy free-text path runs."""
    monkeypatch.setenv("PYTHON_AGENT_LLM_BASE_URL", "http://planner.test/v1")
    monkeypatch.setenv("PYTHON_AGENT_LLM_API_KEY", "test-key")
    monkeypatch.setenv("PYTHON_AGENT_LLM_MODEL", "test-model")
    monkeypatch.setenv("PYTHON_AGENT_PLANNER_STRUCTURED_ENABLED", "false")
    settings = Settings(_env_file=None)  # hermetic: never read the developer's .env

    # The structured path calls `_build_structured_client` — patch it to
    # fail loudly so any accidental invocation surfaces in the test.
    def _fail(*_a, **_k):
        raise AssertionError("structured path invoked when disabled")

    monkeypatch.setattr("agent.planner._build_structured_client", _fail)

    # Canned response from the legacy ChatOpenAI.
    class _LegacyCannedClient:
        async def ainvoke(self, messages, config=None):
            class _Resp:
                content = '{"steps":[{"id":"a","kind":"llm","prompt":"hi"}]}'

            return _Resp()

    monkeypatch.setattr(
        "agent.planner.ChatOpenAI", lambda **_kwargs: _LegacyCannedClient()
    )

    state = new_state(
        user_id=1,
        run_id="r1",
        model="test-model",
        messages=[{"role": "user", "content": "hi"}],
    )
    out_state = await_call(plan_node(state, settings, _NoopTracer()))

    # The structured binding was not invoked — so the legacy path produced a Plan
    # and the planner node appended a `plan` event (not `error`).
    kinds = [e.kind for e in out_state["events_so_far"]]
    assert "plan" in kinds
    assert "error" not in kinds
    assert isinstance(out_state.get("plan"), Plan)


def test_negative_planner_retry_max_refuses_settings(monkeypatch):
    """Settings refuse to boot when PYTHON_AGENT_PLANNER_RETRY_MAX is negative."""
    monkeypatch.setenv("PYTHON_AGENT_LLM_BASE_URL", "http://planner.test/v1")
    monkeypatch.setenv("PYTHON_AGENT_LLM_API_KEY", "test-key")
    monkeypatch.setenv("PYTHON_AGENT_LLM_MODEL", "test-model")
    monkeypatch.setenv("PYTHON_AGENT_PLANNER_RETRY_MAX", "-1")
    with pytest.raises(Exception) as exc_info:
        Settings()
    assert "PYTHON_AGENT_PLANNER_RETRY_MAX" in str(exc_info.value)


def test_append_error_to_messages_appends_to_user_message():
    from langchain_core.messages import HumanMessage, SystemMessage

    msgs = [
        SystemMessage(content="system instructions"),
        HumanMessage(content="hi"),
    ]
    out = _append_error_to_messages(msgs, "steps.0.id: Field required")
    assert out[-1].content.startswith("hi")
    assert PREVIOUS_ATTEMPT_FAILED_HEADER in out[-1].content
    assert "Field required" in out[-1].content


def test_extract_structured_error_normalizes_validation():
    real_error = ValidationError.from_exception_data(
        title="Plan",
        line_errors=[
            {
                "type": "missing",
                "loc": ("steps", 0, "id"),
                "msg": "Field required",
                "input": {},
            }
        ],
    )
    text = _extract_structured_error(real_error)
    assert "Field required" in text


def test_extract_structured_error_normalizes_output_parser():
    # langchain-core 1.6.9 stores the parser message in args[0]; str(exc)
    # appends a troubleshooting doc URL which we strip before injecting.
    exc = OutputParserException("steps.0.kind: bad enum", observation="raw text")
    text = _extract_structured_error(exc)
    assert "bad enum" in text
    assert "For troubleshooting" not in text


def await_call(coro):
    """Run an async coroutine synchronously. Each call gets a fresh loop."""
    import asyncio

    return asyncio.run(coro)

# --- web-tools prompt block (openspec/changes/agent-web-tools, task 6.1) ---


def _web_settings(**overrides) -> Settings:
    base = {"_env_file": None, "llm_base_url": "http://stub", "metrics_port": 0}
    base.update(overrides)
    return Settings(**base)


def _planner_state():
    return new_state(user_id=1, run_id="r", messages=[], model="m")


def test_planner_prompt_byte_identical_when_web_tools_off():
    """Spec scenario "Default configuration changes nothing": with both
    flags off the composed system prompt is byte-identical to the
    pre-web-tools constant."""
    from agent.planner import PLANNER_SYSTEM_PROMPT, _initial_messages

    settings = _web_settings(web_fetch_enabled=False, web_search_enabled=False)
    msgs = _initial_messages(_planner_state(), settings)
    assert msgs[0].content == PLANNER_SYSTEM_PROMPT


def test_planner_prompt_lists_enabled_web_tools():
    """Spec scenario "Enabling advertises the tools to the planner"."""
    from agent.planner import PLANNER_SYSTEM_PROMPT, _initial_messages

    settings = _web_settings(
        web_fetch_enabled=True,
        web_search_enabled=True,
        tavily_api_key="k",
    )
    prompt = _initial_messages(_planner_state(), settings)[0].content
    assert prompt.startswith(PLANNER_SYSTEM_PROMPT)
    assert "web_fetch" in prompt
    assert "web_search" in prompt


def test_planner_prompt_lists_only_enabled_tool():
    from agent.planner import _initial_messages

    settings = _web_settings(web_fetch_enabled=True, web_search_enabled=False)
    prompt = _initial_messages(_planner_state(), settings)[0].content
    assert "web_fetch" in prompt
    assert "web_search" not in prompt


def _llm_step(step_id="llm_1", prompt="撰写一篇关于最近 feed 的总结"):
    return PlanStep(id=step_id, kind=StepKind.llm, prompt=prompt)


def _write_step(step_id="write_1", tool="create_feed", args=None):
    return PlanStep(
        id=step_id,
        kind=StepKind.tool,
        tool=tool,
        args=args if args is not None else {},
    )


def _composed_create_plan():
    return Plan(
        steps=[
            PlanStep(id="rag_1", kind=StepKind.rag, query="recent feeds"),
            _llm_step(),
            _write_step(
                args={"title": "总结", "description": "{{step:llm_1}}"}
            ),
            _llm_step("llm_2", "已发布。"),
        ]
    )


def _guard(plan, message):
    return _enforce_plan_shape(plan, max_steps=10, latest_user_message=message)


def test_guard_prompt_reconciles_unsure_rule_with_write_mandate():
    assert "an explicit state-change request is not ambiguous" in PLANNER_SYSTEM_PROMPT
    assert "draft-and-report" in PLANNER_SYSTEM_PROMPT
    assert "{{step:<step_id>}}" in PLANNER_SYSTEM_PROMPT


def test_prompt_preserves_web_routing_and_caps_feed_length():
    assert "never use `rag` for those requests" in PLANNER_SYSTEM_PROMPT
    assert "under 300 Chinese characters" in PLANNER_SYSTEM_PROMPT
    assert "`create_feed` / `update_feed`" in PLANNER_SYSTEM_PROMPT
    assert "1000 bytes" in PLANNER_SYSTEM_PROMPT


@pytest.mark.parametrize(
    ("message", "required_tool"),
    [
        ("总结我最近的feed，然后发布出去", "create_feed"),
        ("帮我创建一篇新的feed，写写今天吃的拉面", "create_feed"),
        ("把最近那条关于拉面的feed更新一下", "update_feed"),
        ("删除我最近发的那条feed", "delete_feed"),
        ("别删掉旧的feed，帮我创建一篇新的feed", "create_feed"),
        ("我已经删除了旧的feed，现在帮我创建一篇新的feed", "create_feed"),
    ],
)
def test_guard_rejects_explicit_state_change_without_write_tool(
    message, required_tool
):
    plan = Plan(
        steps=[
            PlanStep(id="rag_1", kind=StepKind.rag, query="recent feeds"),
            _llm_step(),
        ]
    )

    out = _guard(plan, message)

    assert isinstance(out, _PlanError)
    assert out.code == "plan_invalid"
    assert out.detail == "Plan is invalid"
    assert required_tool in out.reason


@pytest.mark.parametrize(
    ("message", "steps"),
    [
        ("你好呀，今天过得怎么样？", [_llm_step(prompt="你好！我很好。")]),
        ("教我怎么黑进别人的社交账号", [_llm_step(prompt="我不能帮助这个请求。")]),
        (
            "今天天气如何？上网搜索看看",
            [PlanStep(id="react_1", kind=StepKind.react, prompt="Look up weather", max_iterations=2)],
        ),
        (
            "帮我查最近我发了什么",
            [
                PlanStep(id="rag_1", kind=StepKind.rag, query="recent feeds"),
                _llm_step(prompt="你最近发了三条 feed。"),
            ],
        ),
        (
            "总结我最近发的feed",
            [
                PlanStep(id="rag_1", kind=StepKind.rag, query="recent feeds"),
                _llm_step(),
            ],
        ),
        (
            "帮我写一篇关于最近feed的总结，看看效果",
            [
                PlanStep(id="rag_1", kind=StepKind.rag, query="recent feeds"),
                _llm_step(),
            ],
        ),
        (
            "帮我写一篇关于最近三个月 feed 的总结草稿，先别发",
            [
                PlanStep(id="rag_1", kind=StepKind.rag, query="recent feeds"),
                _llm_step(),
            ],
        ),
        (
            "总结我的feed，但是不要发布",
            [
                PlanStep(id="rag_1", kind=StepKind.rag, query="recent feeds"),
                _llm_step(),
            ],
        ),
        (
            "总结我的feed，仅供预览",
            [
                PlanStep(id="rag_1", kind=StepKind.rag, query="recent feeds"),
                _llm_step(),
            ],
        ),
        (
            "不要移除我关于旅行的feed，帮我把它们总结成一篇周报",
            [
                PlanStep(id="rag_1", kind=StepKind.rag, query="recent feeds"),
                _llm_step(),
            ],
        ),
        (
            "别删掉我的feed，帮我总结成一篇周报",
            [
                PlanStep(id="rag_1", kind=StepKind.rag, query="recent feeds"),
                _llm_step(),
            ],
        ),
        (
            "我已经删除了那些负面feed，现在帮我总结成一篇周报",
            [
                PlanStep(id="rag_1", kind=StepKind.rag, query="recent feeds"),
                _llm_step(),
            ],
        ),
        (
            "我刚刚删了那条负面feed，现在帮我总结成一篇周报",
            [
                PlanStep(id="rag_1", kind=StepKind.rag, query="recent feeds"),
                _llm_step(),
            ],
        ),
        (
            "我刚才删了那条feed，现在帮我把它总结成周报",
            [
                PlanStep(id="rag_1", kind=StepKind.rag, query="recent feeds"),
                _llm_step(),
            ],
        ),
        (
            "我刚删掉了那条feed，帮我总结成一篇周报",
            [
                PlanStep(id="rag_1", kind=StepKind.rag, query="recent feeds"),
                _llm_step(),
            ],
        ),
        (
            "怎么删除feed？我想先了解再决定",
            [
                PlanStep(id="rag_1", kind=StepKind.rag, query="recent feeds"),
                _llm_step(),
            ],
        ),
        (
            "别修改我的feed，帮我总结成一篇周报",
            [
                PlanStep(id="rag_1", kind=StepKind.rag, query="recent feeds"),
                _llm_step(),
            ],
        ),
        (
            "无需更新我的feed，帮我总结成一篇周报",
            [
                PlanStep(id="rag_1", kind=StepKind.rag, query="recent feeds"),
                _llm_step(),
            ],
        ),
        (
            "不用创建新的feed，帮我总结",
            [
                PlanStep(id="rag_1", kind=StepKind.rag, query="recent feeds"),
                _llm_step(),
            ],
        ),
    ],
)
def test_guard_does_not_fire_on_non_state_change(message, steps):
    out = _guard(Plan(steps=steps), message)

    assert isinstance(out, Plan)


@pytest.mark.parametrize(
    "message",
    [
        "不要移除我关于旅行的feed，帮我把它们总结成一篇周报",
        "别删掉我的feed，帮我总结成一篇周报",
        "我已经删除了那些负面feed，现在帮我总结成一篇周报",
        "我刚刚删了那条负面feed，现在帮我总结成一篇周报",
        "我刚才删了那条feed，现在帮我把它总结成周报",
        "我刚删掉了那条feed，帮我总结成一篇周报",
        "怎么删除feed？我想先了解再决定",
        "别修改我的feed，帮我总结成一篇周报",
        "无需更新我的feed，帮我总结成一篇周报",
        "不用创建新的feed，帮我总结",
    ],
)
def test_state_change_intents_ignores_non_request_mentions(message):
    assert _state_change_intents(message) == []


def test_guard_reports_all_missing_write_intents():
    plan = Plan(
        steps=[
            PlanStep(id="rag_1", kind=StepKind.rag, query="recent feeds"),
            _llm_step(),
        ]
    )

    out = _guard(plan, "更新并删除我的feed")

    assert isinstance(out, _PlanError)
    assert "update_feed" in out.reason
    assert "delete_feed" in out.reason


def test_guard_does_not_fire_without_latest_user_message():
    plan = Plan(
        steps=[
            PlanStep(id="rag_1", kind=StepKind.rag, query="recent feeds"),
            _llm_step(),
        ]
    )

    out = _enforce_plan_shape(plan, max_steps=10)

    assert isinstance(out, Plan)


def test_guard_ignores_earlier_turns_when_selecting_the_message():
    messages = [
        {"role": "user", "content": "帮我发布一篇 feed"},
        {"role": "assistant", "content": "好的"},
        {"role": "user", "content": "你好呀"},
    ]

    assert _latest_user_message(messages) == "你好呀"


def test_guard_rejects_create_without_preceding_composing_llm():
    plan = Plan(
        steps=[
            PlanStep(id="rag_1", kind=StepKind.rag, query="拉面"),
            _write_step(
                args={"title": "拉面", "description": "拉面和咖喱的总结"}
            ),
            _llm_step("llm_1", "已发布。"),
        ]
    )

    out = _guard(plan, "总结我最近的feed并发布")

    assert isinstance(out, _PlanError)
    assert out.code == "plan_invalid"
    assert "preceded by an llm step" in out.reason


def test_guard_rejects_placeholder_write_args_without_reference():
    plan = Plan(
        steps=[
            PlanStep(id="rag_1", kind=StepKind.rag, query="拉面"),
            _llm_step(),
            _write_step(
                args={"title": "总结", "description": "拉面和咖喱"}
            ),
            _llm_step("llm_2", "已发布。"),
        ]
    )

    out = _guard(plan, "总结我最近的feed并发布")

    assert isinstance(out, _PlanError)
    assert out.code == "plan_invalid"
    assert "{{step:<step_id>}}" in out.reason
    assert "'llm_1'" in out.reason


def test_guard_accepts_composed_write_with_reference():
    out = _guard(_composed_create_plan(), "总结我最近的feed并发布")

    assert isinstance(out, Plan)
    assert out.steps[2].args["description"] == "{{step:llm_1}}"


@pytest.mark.parametrize(
    ("message", "tool"),
    [
        ("把最近那条feed更新一下", "update_feed"),
        ("把最近的feed都更新一下", "update_feeds"),
        ("删除我最近发的那条feed", "delete_feed"),
        ("删除我最近发的所有feed", "delete_feeds"),
    ],
)
def test_guard_accepts_matching_mutation_sibling(message, tool):
    plan = Plan(
        steps=[
            _write_step("write_1", tool, {"feed_ids": [1]}),
            _llm_step(prompt="已完成。"),
        ]
    )

    out = _guard(plan, message)

    assert isinstance(out, Plan)


def test_guard_missing_write_needs_composition_shaped_final_llm():
    plan = Plan(
        steps=[
            PlanStep(id="tool_1", kind=StepKind.tool, tool="list_feeds", args={}),
            _llm_step("llm_1", "你最近发了三条 feed。"),
        ]
    )

    out = _guard(plan, "把最近那条feed更新一下")

    assert isinstance(out, Plan)


def test_guard_auto_appended_answer_is_not_composition():
    plan = Plan(
        steps=[
            PlanStep(id="tool_1", kind=StepKind.tool, tool="list_feeds", args={}),
        ]
    )

    out = _guard(plan, "帮我发布一条新的feed")

    assert isinstance(out, Plan)
    assert out.steps[-1].prompt == _AUTO_ANSWER_PROMPT


def test_guard_rejection_reprompts_and_recovers(monkeypatch, scripted_settings):
    bad = Plan(
        steps=[
            PlanStep(id="rag_1", kind=StepKind.rag, query="recent feeds"),
            _llm_step(),
        ]
    )
    good = _composed_create_plan()
    client = _ScriptedStructuredClient(script=["ok", "ok"], successes=[bad, good])
    monkeypatch.setattr("agent.planner._build_structured_client", lambda _s: client)

    state = new_state(
        user_id=1,
        run_id="r1",
        model="test-model",
        messages=[{"role": "user", "content": "总结我最近的feed并发布"}],
    )
    plan_or_error = await_call(
        _emit_structured_plan(state, scripted_settings, _NoopTracer())
    )

    assert isinstance(plan_or_error, Plan)
    assert client._call_count == 2
    retry_prompt = client._captured_messages[1][-1].content
    assert PREVIOUS_ATTEMPT_FAILED_HEADER in retry_prompt
    assert "create_feed" in retry_prompt


def test_guard_rejection_exhaustion_returns_plan_invalid(monkeypatch, scripted_settings):
    bad = Plan(
        steps=[
            PlanStep(id="rag_1", kind=StepKind.rag, query="recent feeds"),
            _llm_step(),
        ]
    )
    client = _ScriptedStructuredClient(script=["ok", "ok"], successes=[bad, bad])
    monkeypatch.setattr("agent.planner._build_structured_client", lambda _s: client)

    state = new_state(
        user_id=1,
        run_id="r1",
        model="test-model",
        messages=[{"role": "user", "content": "总结我最近的feed并发布"}],
    )
    plan_or_error = await_call(
        _emit_structured_plan(state, scripted_settings, _NoopTracer())
    )

    assert isinstance(plan_or_error, _PlanError)
    assert plan_or_error.code == "plan_invalid"
    assert "create_feed" in plan_or_error.reason
    assert client._call_count == 2
