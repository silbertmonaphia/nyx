# Tasks

## 1. Step cursor and one-step executor node

- [x] 1.1 Add the `step_cursor: int` channel to `AgentState` and `new_state` (default 0); verify with a `state` unit test that new and legacy-shaped states read the cursor as 0 when absent
- [x] 1.2 Split the executor loop into a one-step `executor` node: run at most one plan step per invocation, advance the cursor once (including on a failed step and on the defensive `step_outputs` skip), and keep the end-of-plan `_maybe_emit_partial_failure` call inside the last step's invocation; verify with an executor unit test that asserts one step per call, cursor advance on ok/failed/skipped, and the partial-failure note still precedes the final answer's deltas
- [x] 1.3 Add the post-executor router: `END` when `terminal_status != "ok"` or `cursor >= len(plan.steps)`, else `executor`; keep the planner → executor edge unconditional so the `plan is None` defensive branch is unchanged; verify with graph-level tests that a 3-step plan runs exactly three executor supersteps and a budget termination stops the loop
- [x] 1.4 Pass `durability="sync"` to `astream` only when the graph carries a checkpointer (the installed langGraph exposes durability per invocation, not on `compile()`); verify the graph builds with and without a checkpointer and that existing no-checkpointer tests (`test_run.py`, `test_graph*.py`) pass

## 2. Per-step checkpoint durability and resume

- [x] 2.1 Keep the defensive `step_outputs` skip as the legacy-checkpoint path and read the cursor with `.get("step_cursor", 0)`; add a DB-backed test that seeds a two-node-graph checkpoint (pending `executor`, no cursor, partial outputs) and asserts resume completes without re-running the captured steps
- [x] 2.2 Rewrite `test_run_resume.py` so step 1 performs a real await before writing its output (the old bug's timing), cancel during step 2, resume from a fresh graph/checkpointer on the same DB, and assert step 1 is not re-invoked while step 2 re-runs; verify with `TEST_ASYNC_PG_DSN` set
- [x] 2.3 Assert the durability contract directly: while step 2 is blocked, `aget_state` already reports step 1's output and the cursor at 1 (sync durability); verify in the same DB-backed test
- [x] 2.4 Update `test_graph_persistence.py`, `test_detach_stream.py`, and any test that assumed a single executor chunk or a two-node `values` stream; verify with `make -C backend/agent_py test`, a second bare-config run without `TEST_ASYNC_PG_DSN`, and `make -C backend/agent_py test-cov`

## 3. Behavior regression and docs

- [x] 3.1 Verify the terminal, approval, ReAct-cap, arg-reference, and tool-retry paths are unchanged: run the existing executor/approval/retry tests and add a resume-during-approval test asserting the re-run step re-emits `confirm_request` and the stale approval resolves as expired
- [x] 3.2 Document the per-step checkpoint/resume contract in `backend/agent_py/README.md` and the resume bullet in `FUTURE_AGENT.md`; verify every documented behavior matches the code and the delta spec

## 4. Browser-level end-to-end

- [x] 4.1 Add a live Playwright spec (serial, `@live`) that sends a prompt planning a deterministic tool step followed by an answer step, waits for the tool step to render, `SIGKILL`s the side-stack agent via its pidfile, restarts it, and lets the SPA recovery ladder re-POST the echoed `run_id`; asserts the run completes with no chat error and that the tool-call metric (`agent_tool_calls_total` on the side agent's metrics port) and the step trace show no second execution of the completed step; verify with `npm run test:e2e:live -- --workers=1` against the side stack
- [x] 4.2 Confirm the non-live suite is unaffected (`npm run test:e2e`) and the live spec is included in the `test:e2e:live` selection; record the agent-restart prerequisite in the `backend/agent_py/README.md` resume section (a spec-header comment is barred by the no-comments rule)
