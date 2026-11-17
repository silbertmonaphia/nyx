# Proposal

## Why

The agent graph has exactly two supersteps — `planner` and `executor` — so LangGraph checkpoints only at those boundaries. The executor iterates `plan.steps` inside one node, which means a crash mid-executor leaves the last durable checkpoint at the post-planner state: resume re-runs the executor from the top of the plan, re-spending tokens and re-invoking tools for steps that already completed. The executor's `step_outputs` skip only helps when the async checkpoint writer happens to serialize the in-flight, in-place-mutated channel values after a step completed — a timing artifact, not a contract. The existing `test_run_resume.py` passes because its stub's first step performs no I/O; on a real run the first tool/LLM await is exactly when the writer serializes, so completed steps are not captured.

The current `agent-python-service` spec already says resume re-executes no completed steps; today it is only true by luck. This change makes it structural. It complements, and does not replace, `mcp-tool-call-idempotency`: an in-flight step must always re-run after a crash, so its side effects still need the deterministic idempotency key.

## What Changes

- Split the executor's single-node `for step in plan.steps` loop into per-step LangGraph execution: each plan step runs in its own superstep, backed by a loop edge (planner → step → step → … → END).
- Add an explicit step cursor to `AgentState` (derived state cannot distinguish "step failed" from "step not yet run"; failed steps still advance, exactly as today).
- Resume reads the last per-step checkpoint and continues at the first unfinished step. Only the in-flight step (the one that was running when the process died) re-runs; its side effects are covered by `mcp-tool-call-idempotency`.
- Preserve the existing behavior of every step handler: budget termination, tool dispatch retries, approval gate pauses, ReAct iteration cap, arg-reference resolution, partial-failure notes, and the `step_start` / `tool_call` / `tool_result` / `step_end` / `delta` frame sequence.
- Keep the wire contract unchanged: same frame kinds, order, and payloads; resumed runs keep replaying already-checkpointed frames as they do today.
- Replace the timing-dependent resume tests with tests that exercise a real I/O await mid-step and assert that completed steps are not re-executed while the in-flight step is.
- No **BREAKING** changes; no API, SSE, database, or configuration changes.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `agent-python-service`: the resume requirement becomes a per-step durability contract — each plan step is a checkpoint boundary, completed steps are never re-executed after a crash, and only the in-flight step re-runs.

## Impact

- `backend/agent_py/agent/graph.py` — node wiring, loop edge, and routing.
- `backend/agent_py/agent/executor.py` — single-step node around the existing handlers; remaining-step / terminal routing.
- `backend/agent_py/agent/state.py` — step cursor channel and `new_state`.
- `backend/agent_py/agent/main.py` — only if terminal-frame emission or chunk draining needs adjustment for the additional supersteps.
- Tests: `backend/agent_py/tests/test_run_resume.py`, `test_graph_persistence.py`, `test_run.py`, `test_detach_stream.py`; Playwright `frontend/tests/e2e/chat-stream-resume.spec.ts` / `chat-step-replay.spec.ts`.
- Docs: `backend/agent_py/README.md`, `FUTURE_AGENT.md` where the resume story is described.
- No migrations, no new dependencies, no rollout gates (implementation-only change; no sibling `<change-name>-rollout`).
- Sequenced after `openspec/changes/mcp-tool-call-idempotency/` so the in-flight step's replayed side effects are already deduped when this lands.
