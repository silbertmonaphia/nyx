# Proposal

## Why

A failed step is invisible to the model that writes the final answer. `_render_prior_data` only carries successful `step_outputs` into later steps, so after a `tool` step fails — most visibly when an approval expires before a decision — the answering `llm` step receives the user's request and nothing about the failure. The small default model then often presents the run as successful, and the SPA's next request resends that false text as history. A bare "继续 / 再试试" therefore has nothing to act on: the previous tool call and its error are gone.

The partial-failure banner already tells the user the run diverged; the answer text currently can contradict it. This change makes the model-facing answer agree with the banner and lets the next turn's text history carry the real failure reason.

## What Changes

- Executor: every model-facing step (`llm`, `react`) that follows a failed step receives a bounded "failed steps" context section built from terminal `step_end{status="failed"}` events and the already wire-safe `tool_result.error` text, plus a static instruction to report the failure plainly and not claim the failed action succeeded. When no step failed, prompts are byte-identical to today.
- Planner: a bare retry instruction ("再试试 / 重试 / 继续 …") whose immediately preceding assistant message names a feed write tool (`create_feed` / `update_feed` / `delete_feed`) as not executed re-plans that tool, following the existing compose-before-write rules. Without both signals, planning is unchanged.
- No SSE frame shapes, request shapes, or history schema change. The partial-failure banner is unchanged; the answer text becomes consistent with it.
- Recovery remains user-driven: the retried write still goes through a fresh confirmation gate, and a user denial is not auto-retried.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `agent-partial-failure`: failed steps become visible to later model-facing steps in the same run, not only to the UI banner.
- `agent-plan-shape-policy`: add retry-intent routing that re-plans a write tool the previous answer reported as not executed.

## Impact

- Code: `backend/agent_py/agent/executor.py` (prior-data rendering, answer/react prompt assembly), `backend/agent_py/agent/planner.py` (system prompt rule, retry cue set, write-routing guard).
- Tests: `backend/agent_py/tests/test_executor.py`, planner tests, plus a frontend e2e covering the user-visible answer text (`frontend/tests/e2e/chat-feed-approval.spec.ts` and its `@live` sibling).
- Docs: `FUTURE_AGENT.md` if the behavior earns a roadmap note; agent README only if prompt behavior is documented there.
- No rollout gates foreseen: the work is completable in the dev/CI environment, so no sibling rollout change is created.
