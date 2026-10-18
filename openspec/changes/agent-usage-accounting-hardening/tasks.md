# Tasks

## 1. Resume usage reconciliation

- [ ] 1.1 In `agent/state.py`, treat a state without `cumulative_prompt_tokens` as a pre-upgrade checkpoint: the first provider-usage accrual resets `cumulative_tokens` to that delta instead of adding it to the legacy estimate; verify with a unit test that a legacy state plus one 120/30 usage yields `total_tokens=150`
- [ ] 1.2 Extend `tests/test_usage_accounting.py` with the resumed-run cases: a second provider-reported call after the reset accumulates normally, and the assembled `done` frame satisfies `total_tokens == prompt_tokens + completion_tokens`; verify the new tests fail against the pre-fix `_add_usage`
- [ ] 1.3 Run `cd backend/agent_py && .venv/bin/python -m pytest tests/test_usage_accounting.py -q` and confirm the full file passes

## 2. Callback preservation regression test

- [ ] 2.1 Add a test in `tests/test_planner.py` that invokes the structured planner through a real graph node with an ambient sentinel callback handler (via `var_child_runnable_config`), asserting the sentinel fires alongside `UsageMetadataCallbackHandler`; verify the test fails if the usage handler is passed as a standalone `callbacks` config instead of merged
- [ ] 2.2 Run `cd backend/agent_py && .venv/bin/python -m pytest tests/test_planner.py -q` and confirm the full file passes

## 3. Integration verification

- [ ] 3.1 Run the full agent suite in both configurations: `cd backend/agent_py && .venv/bin/python -m pytest tests/ -q` and the same command with `TEST_ASYNC_PG_DSN` set; verify both pass
- [ ] 3.2 Run `ruff check` and `mypy` on the touched modules; verify no new diagnostics beyond the recorded baseline
- [ ] 3.3 Run `frontend/` `npm run test:e2e:live -- chat-token-usage --workers=1` against the side stack and host vLLM; verify the rendered usage line still satisfies `total == prompt + completion` on a fresh run
