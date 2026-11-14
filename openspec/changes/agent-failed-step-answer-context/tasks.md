# Tasks

## 1. Executor: failed-step context reaches later model-facing steps

- [ ] 1.1 Add a helper that derives failed-step sections for steps ordered before the current step from `step_end{status: "failed"}` events, using the terminal `tool_result{ok: false}.error` for tool steps and a static description for other step kinds; verify with unit tests covering tool and llm failures, plan ordering, and multiple failures in `backend/agent_py/tests/test_executor.py`
- [ ] 1.2 Render the failed-step sections through `_render_prior_data` under the existing per-section cap, total cap, and marker stripping; verify the existing cap tests still pass and a clean run's rendered block stays empty
- [ ] 1.3 Add the static no-false-success instruction as a separate system message in both `_run_llm` and `_run_react` whenever an earlier step failed, requiring the model to quote a not-executed error verbatim; verify captured-model-input tests for both step kinds
- [ ] 1.4 Verify the clean-run prompt invariant: with no failures, the assembled message list for `llm` and `react` matches the pre-change form byte for byte (`test_answer_prompt_is_byte_identical_without_prefs` and siblings)

## 2. Planner: retry instruction re-plans a reported write

- [ ] 2.1 Add the Chinese retry cue set, the immediately-preceding-assistant not-executed detection (write tool name plus 未执行 / 没有执行 / not executed), and negation suppression; extend `_enforce_write_routing` to mandate the named write tool when both signals hold and reject the plan otherwise, with the existing compose-before-write rules unchanged; verify fire / no-fire / negation cases with unit tests in the planner test module
- [ ] 2.2 Add the matching rule to the planner system prompt so the model plans the retry before the deterministic guard runs; verify a prompt-content test and a fake-LLM planning test
- [ ] 2.3 Add an executor-level test that runs a plan whose `create_feed` confirmation expires, asserts the answering model input carries the denial text and instruction, then builds the next turn's messages from the persisted answer text plus 再试试 and asserts the planner emits a `create_feed` plan and dispatch raises a fresh approval request; verify with `pytest`

## 3. Cross-runtime and user-visible coverage

- [ ] 3.1 Add an agent SSE integration test with a stubbed LLM that drives an expired confirmation through the stream and asserts the final `delta` text reports the failure while the frame sequence and shapes are unchanged; verify with `pytest`
- [ ] 3.2 Update the mocked frontend approval spec (`frontend/tests/e2e/chat-feed-approval.spec.ts`) so its fixture's final delta carries the failure summary and the panel renders it; verify with `npm run test:e2e -- chat-feed-approval`
- [ ] 3.3 Extend the live approval spec (`frontend/tests/e2e/chat-feed-approval-live.spec.ts`) to expire a confirmation and assert the final answer text mentions the failed write and does not claim success; verify with `npm run test:e2e:live -- --workers=1` against the live stack

## 4. Documentation

- [ ] 4.1 Update `docs_agent/AgentFaultTolerance.md` so the tool-failure and provider-failure rows and the summary state that the answering step now receives the safe failure text
- [ ] 4.2 Add the landed behavior to `FUTURE_AGENT.md` and align the failure-handling wording in `README_FULL.md` / `README.zh.md` if it now understates the behavior; verify the described behavior matches the shipped code

## 5. Integration gates

- [ ] 5.1 Run `make -C backend/agent_py lint`, `make -C backend/agent_py test`, and `make -C backend/agent_py test-cov`; verify all exit zero
- [ ] 5.2 Run `cd frontend && npm run lint && npm test && npm run test:e2e`; verify all exit zero
- [ ] 5.3 Run `python3 scripts/check_no_comments.py` on the change diff; verify it exits zero
