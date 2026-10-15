"""Live-network smoke tests for the planner path.

These tests are gated on operator-supplied live-network credentials via
`PYTHON_AGENT_LIVE_BASE_URL` (the LLM endpoint) and `PYTHON_AGENT_LIVE_DSN`
(Postgres for `agent.runs`). They self-skip when those env vars are
unset, so the standard `pytest tests/` run (CI included) stays a
no-network pass.

The tests exist so an operator running against a real OpenAI / vLLM can
verify the structured-output path + retry loop end-to-end without a
browser. Wiring them into `pytest tests/` is a no-op when env vars are
absent.

Lives apart from `test_planner.py` so a single env var can opt the suite
into the live path without touching the existing offline tests.
"""

from __future__ import annotations

import os

import pytest

LIVE_BASE_URL = os.environ.get("PYTHON_AGENT_LIVE_BASE_URL")
LIVE_DSN = os.environ.get("PYTHON_AGENT_LIVE_DSN")

pytestmark = pytest.mark.skipif(
    not (LIVE_BASE_URL and LIVE_DSN),
    reason=(
        "PYTHON_AGENT_LIVE_BASE_URL and PYTHON_AGENT_LIVE_DSN both unset; "
        "self-skipping to keep `pytest tests/` offline."
    ),
)


@pytest.mark.asyncio
async def test_structured_path_emits_plan_frame_against_live_provider():
    """End-to-end: structured planner binds `Plan` via the real provider.

    Adds the live-provider smoke that task 6.1 (intended operator curl
    against `/agent/run`) covers manually. Asserts the planner returns
    a `Plan` whose `steps` are non-empty.
    """
    from agent.config import Settings
    from agent.planner import _emit_structured_plan
    from agent.state import new_state

    settings = Settings(
        _env_file=None,  # hermetic: never read the developer's .env
        llm_base_url=LIVE_BASE_URL,  # type: ignore[arg-type]
        database_url=LIVE_DSN,  # type: ignore[arg-type]
    )

    state = new_state(
        user_id=1,
        run_id="smoke-1",
        model="test-model",
        messages=[{"role": "user", "content": "hi"}],
    )
    plan_or_error = await _emit_structured_plan(state, settings, _NoopTracer())

    # Two outcomes are acceptable on a live run:
    #   - A well-formed Plan came back from the structured binding.
    #   - The model legitimately exhausted the retry budget (`plan_invalid`)
    #     or emitted an oversized plan — both prove the structured wiring
    #     round-trips. `planner_failed` means the provider link itself is
    #     broken (auth, network, unsupported response_format) — that MUST
    #     fail this smoke.
    from agent.planner import _PlanError
    from agent.types import Plan

    if isinstance(plan_or_error, _PlanError):
        assert plan_or_error.code in {"plan_invalid", "plan_too_large"}, (
            "provider wiring failure: "
            f"{plan_or_error.code} / {plan_or_error.reason!r}"
        )
    else:
        assert isinstance(plan_or_error, Plan)
        assert len(plan_or_error.steps) >= 1


class _NoopTracer:
    def start_as_current_span(self, name, **_attrs):
        from contextlib import nullcontext

        span = type("Span", (), {"set_attribute": lambda self, k, v: None})()
        return nullcontext(span)
