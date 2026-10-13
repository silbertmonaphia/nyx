# Tasks

> **Rollout split (2026-10-13):** rollout-gated items (live smokes, Playwright e2e against the running stack, prod env flip, 1-week CI-green wait) live in `openspec/changes/agent-tool-dispatch-retry-rollout/`. Items here are dev/CI-only.

## 1. Settings

- [ ] 1.1 Add `tool_retry_max: int = 2` and `tool_retry_backoff_base_ms: int = 250` and `tool_retry_backoff_ceiling_ms: int = 5000` to `backend/agent_py/agent/config.py`. Pydantic validator rejects `tool_retry_max < 0` at startup. Verify by booting `make dev-agent` and asserting a `PYTHON_AGENT_TOOL_RETRY_MAX=-1` startup refuses to start with a clear error naming the offending env var.
- [ ] 1.2 Document the three new env vars in `backend/agent_py/README.md` (env-var table) and `backend/agent_py/.env.example` (commented). `grep -n 'PYTHON_AGENT_TOOL_RETRY' backend/agent_py/README.md backend/agent_py/.env.example` finds all three.

## 2. Counter

- [ ] 2.1 Register `agent_tool_retries_total{tool, reason}` on the Prometheus exporter in `backend/agent_py/agent/observability.py` alongside `agent_planner_parse_failures_total`. Reason label is the closed enumeration `tool_arg_invalid` / `tool_execution_transient`. Verify by `curl :9001/metrics | grep agent_tool_retries_total` showing the registered metric (live check gated on rollout change §1.1; the unit-level coverage in §4 is the dev/CI gate).
- [ ] 2.2 Add `agent_tool_retries_total{tool, reason}.inc()` call sites in the executor's two retry paths (one for tool-arg retries, one for transient retries). Each retry attempt increments by exactly 1; the first attempt does not increment. Verify by `pytest test_executor.py` covering both increment paths.

## 3. Tool-arg retry path

- [ ] 3.1 In `backend/agent_py/agent/executor.py`, wrap the `tools.call(tc_name, args)` site (line ~296) in a retry loop. The registry's typed validation error is caught, the model is re-invoked with the validator's error appended under `PREVIOUS_ATTEMPT_FAILED`, the new model's tool call is dispatched, and the loop continues up to `settings.tool_retry_max` total attempts. The retry budget is per-step and per-tool — a fresh step starts a fresh budget.
- [ ] 3.2 Add `retry_count: int = 0` to `ToolResultEvent` in `backend/agent_py/agent/types.py` (optional, defaults to 0). The final `tool_result` frame carries the cumulative retry count for the call (0 on first-try success, N when N retries happened). Wire-contract test: round-trip a `tool_result` frame with `retry_count=2` through the actual `chatService.ts` parser (frontend) to confirm the additive field doesn't break the existing parser. Verify by `pytest backend/agent_py/tests/test_executor.py` covering the wire shape.
- [ ] 3.3 Add `test_executor_retries_on_registry_validation_failure` — first dispatch raises a typed validation error, second succeeds. Assert the run-state's `tool_result` carries `retry_count=1` and `agent_tool_retries_total{tool=<name>, reason="tool_arg_invalid"}` increments by 1.
- [ ] 3.4 Add `test_executor_terminates_step_on_persistent_validation_failure` — every dispatch raises validation error. Assert the step ends with `step_failed`, the final `tool_result` carries the last validator's error mapped through the safe funnel (no internal text), and `agent_tool_retries_total{tool, reason="tool_arg_invalid"}` increments by exactly `tool_retry_max` (not `tool_retry_max + 1`).
- [ ] 3.5 Add `test_per_step_budget_isolates_retry_state` — two steps, each exhausting its own tool-arg retry budget. Assert step 2's budget is independent (exhaustion in step 1 does not prevent retries in step 2).
- [ ] 3.6 Add `test_retry_reprompt_contains_validator_error` — the re-prompted model call's message list ends with the assistant message that emitted the bad call, the rejected `tool_result` frame, and a final user message quoting the validator's error and the tool's argument schema.

## 4. Transient-failure retry path

- [ ] 4.1 In `backend/agent_py/agent/executor.py`, add a second retry loop around the same `tools.call` site for transient errors (network timeout, 5xx, 408, 429). Backoff: `min(base * 2^attempt + jitter, ceiling)` with `base=250ms`, `ceiling=5s`, jitter ±20%. The two retry paths MUST NOT both fire on the same error — a validation error routes to the tool-arg path, a transport error routes here.
- [ ] 4.2 Add `test_executor_retries_on_transient_failure` — first `tools.call` raises a network timeout, second succeeds. Assert `agent_tool_retries_total{tool, reason="tool_execution_transient"}` increments by 1 and the eventual `tool_result` carries `retry_count=1`.
- [ ] 4.3 Add `test_executor_terminates_step_on_persistent_transient_failure` — every `tools.call` raises a 503. Assert the step ends with `step_failed`, the mapped error reaches the user (no internal text), and the counter increments by `tool_retry_max` (not `+1`).
- [ ] 4.4 Add `test_4xx_non_retryable_fails_fast` — `tools.call` raises a 400. Assert no retry occurs, `agent_tool_retries_total` is not incremented, the step fails immediately.
- [ ] 4.5 Add `test_validation_error_routes_to_arg_path_not_transient` — a typed validation error MUST NOT trigger the transient retry loop, even if it happens to be raised at the transport layer (e.g. the bridge returns 422 with a Pydantic error body). The classifier lives in the executor, not the transport.

## 5. Cross-runtime wire contract

- [ ] 5.1 Add a wire-contract test in `backend/agent_py/tests/test_executor.py` (or a new `test_tool_result_retry_count.py`) that round-trips a `tool_result` frame with `retry_count=2` through the actual `chatService.ts` SSE parser, not just asserts the producer's in-process shape. The test is a Python-side fixture of the parser's expected behavior, modeled on the existing `test_planner_parity.py` cross-provider pattern. Verify by the test passing.

## 6. E2E browser test

- [ ] 6.1 Add `frontend/tests/e2e/chat-agent-retry.spec.ts` modeled on `frontend/tests/e2e/chat-agent-planner.spec.ts`. The test drives a chat that triggers a tool-arg retry (via a controllable provider stub or a known-flaky tool) and asserts: the `tool_result` row carries the retry metadata, the step reaches a terminal status, no error frame is emitted, and the final answer is non-empty. The spec is authored here; execution against the live stack is gated on rollout change §2.1.
- [ ] 6.2 Wire the spec into `frontend/playwright.config.ts` (`testDir: './tests/e2e'`). Verify by `npx playwright test --list` listing the new spec. Collection-only gate; live execution is rollout-gated.

## 7. Docs

- [ ] 7.1 Tick `FUTURE_AGENT.md §1.3` "Retry on malformed tool args" and "Retry on tool execution failure" bullets with rationale pointing at this change. `grep -n 'agent-tool-dispatch-retry' FUTURE_AGENT.md` ✓.
- [ ] 7.2 Update `backend/agent_py/README.md` with the three new env vars and the new counter name (paired with §1.2).
- [ ] 7.3 Update `backend/agent_py/.env.example` with the three new vars (commented) (paired with §1.2).

## 8. Integration checks

- [ ] 8.1 Run the full Python pytest suite: `cd backend/agent_py && .venv/bin/python -m pytest tests/ -q` → green. Use the project venv (the pyenv default python lacks `asyncpg`, which silently changes collection).
- [ ] 8.2 Run the Go test suite: `cd backend && SKIP_CONTAINERS=true go test ./...` — green. The Go side is untouched; this is a regression check.
- [ ] 8.3 Lint the Python side: `.venv/bin/ruff check …` and `.venv/bin/mypy agent`. Verify: no new findings on files this change touches.
- [ ] 8.4 `openspec validate --change agent-tool-dispatch-retry` reports the change as valid. `openspec status --change agent-tool-dispatch-retry` shows every artifact `done`.

## 9. OpenSpec rollout sibling

- [x] 9.1 (already done in this session) Created `openspec/changes/agent-tool-dispatch-retry-rollout/` with `skip_specs: true`. Modeled on `openspec/changes/agent-planner-structured-rollout/tasks.md`. Verify by `openspec list --json` showing both changes and `openspec status --change agent-tool-dispatch-retry-rollout` showing tasks-only.

## 10. Open question resolution (from design.md)

- [x] 10.1 **Resolved 2026-10-13** — per-step, per-tool budget applies across the entire `kind="react"` step (not per ReAct iteration). `design.md` Open Questions updated. Add `test_react_step_shares_budget_across_iterations` to `backend/agent_py/tests/test_executor.py`: ReAct iter 1 exhausts the tool-arg retry budget, iter 2's tool call fails immediately with the final `tool_result{ok: false}` and no further retry attempts. Verify by the test passing under both `tool_retry_max=2` and `tool_retry_max=0` (the latter trivially fails every retry). (Test added in §3 / §4 batch — worktree B.)

## 11. Partial-failure signal (backend)

- [ ] 11.1 Extend `NoteEvent` in `backend/agent_py/agent/types.py` with `kind: Literal[..., "partial_failure"]` and `failed_steps: list[str] = []`. The new variant is additive — old consumers that don't switch on `kind` continue to render the existing `note{kind: "resumed" | "fresh_run"}` frames unchanged. Verify by `pytest` of the Pydantic model accepting both old and new shapes.
- [ ] 11.2 Add a single helper `_collect_failed_steps(state: AgentState, settings: Settings) -> list[str]` to `backend/agent_py/agent/executor.py`. The function inspects the run state in this priority order and returns the union of: (a) every `step_id` whose `step_end` was emitted with `status="failed"`; (b) every `step_id` whose final `tool_result` was `ok: false` AND `retry_count == settings.tool_retry_max` (terminal failure, not intermediate retry). RAG-empty results are NOT collected (the step reached `ok: true`); `run_metadata_unavailable` is NOT collected (it's a non-fatal error frame, not a step failure). Verify by `pytest test_executor.py::test_collect_failed_steps_priority` asserting all six scenarios from the spec's priority order.
- [ ] 11.3 In the SSE publisher (wherever `done` and the final `delta` frames are emitted), if `_collect_failed_steps(state, settings)` is non-empty OR the run state carries any `error{code: "plan_too_large" | "plan_invalid" | "react_iterations_exceeded" | "budget_exceeded" | "budget_unavailable"}`, emit `note{kind: "partial_failure", failed_steps: <list>}` immediately before the first `delta` of the final answer. If `failed_steps` is empty (plan-level error), the list is `[]`. Verify by `pytest test_executor.py` covering: (i) tool retry exhausted → banner with that step_id; (ii) intermediate retry failure recovered → no banner; (iii) plan_too_large → banner with `failed_steps: []`; (iv) run_metadata_unavailable with all steps ok → no banner; (v) RAG empty → no banner; (vi) two failed steps → banner with both step_ids in plan order.
- [ ] 11.4 Wire-contract test: round-trip a `note{kind: "partial_failure", failed_steps: ["step_3", "step_5"]}` frame through the actual `chatService.ts` SSE parser (test fixture on the Python side, modeled on the `test_planner_parity.py` pattern). The frame is parsed correctly into the same shape `ChatPanel` consumes. Verify by the parity test passing — this catches producer/consumer drift on the new note variant.

## 12. Partial-failure banner (frontend)

- [ ] 12.1 Add `PartialFailureNote` to `frontend/src/features/chat/types/chat.ts` (`{ kind: "partial_failure"; failedSteps: string[] }`). Extend `chatService.ts` to parse `note{kind: "partial_failure"}` SSE frames into this shape and surface it on the message. Old messages that never had a `partial_failure` note continue to render with no banner. Verify by `vitest` of `chatService.test.ts` covering both parse and non-parse paths.
- [ ] 12.2 Add a `<PartialFailureBanner>` component in `frontend/src/features/chat/components/PartialFailureBanner.tsx`. Props: `failedSteps: string[]`. Renders a yellow-tinted banner with text "部分步骤失败：以下回答可能不完整" (or the project's localised variant) and lists the failed step ids. Renders nothing when `failedSteps` is empty AND the run emitted the banner (defensive — server should not send an empty `failed_steps` unless there was a plan-level error, in which case the text becomes "Run did not reach a terminal step"). Test ids: `data-testid="chat-partial-failure-banner"`, `data-failed-steps` (comma-joined ids).
- [ ] 12.3 In `ChatPanel.tsx`, render `<PartialFailureBanner>` immediately above the assistant message's final-answer `delta` region when the message has a `partialFailure` note. The banner appears WITHOUT requiring the user to expand the `agent-steps` disclosure. Verify by `vitest` of `ChatPanel.test.tsx` covering: (i) a message with `partialFailure: { failedSteps: ["step_2"] }` renders the banner; (ii) a message without `partialFailure` does not render the banner; (iii) the banner is rendered in the DOM regardless of whether the `agent-steps` panel is open.
- [ ] 12.4 Extend `frontend/tests/e2e/chat-agent-retry.spec.ts` (or add a new case) with two browser tests: (a) drive a chat that triggers a tool retry cap exhaustion and assert `data-testid="chat-partial-failure-banner"` is present with `data-failed-steps` containing the failing step id; (b) drive a chat where the tool retries within budget and succeeds, and assert the banner is NOT present. The spec is authored here; execution against the live stack is gated on rollout change §2.x.

## 13. Top-level fault-surface index

- [ ] 13.1 Write `AgentFaultTolerance.md` at the repo root. One table indexing every fault surface in the agent stack (planner parse, tool arg invalid, tool execution transient, budget exceeded, budget unavailable, RAG empty, run-metadata unavailable, plan too large, plan invalid, ReAct iterations exceeded, provider failover, run timeout / partial completion) with: column for "covered by", column for "current behaviour", column for "proposed handling if re-prioritised", column for "user-visible signal". Cross-link this OpenSpec change, `agent-planner-structured`, `agent-run-state`, and `FUTURE_AGENT.md` from the doc. The doc is the entry point for any future fault-tolerance slice — implementations should be able to grep it and find the existing coverage + the unaddressed gap. (Done in this session — verify the file is present and cross-links resolve.)
