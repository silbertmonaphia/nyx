# Tasks

## 1. BudgetGuard reconciliation

- [ ] 1.1 Add `BudgetGuard.reconcile(user_id, hour_key, estimated_tokens, real_tokens, tracer)` — `DECRBY` estimate then `INCRBY` real against the given hour bucket, honoring the key's TTL semantics (no TTL refresh on reconcile writes)
- [ ] 1.2 Surface the charged hour key from `check_and_increment` (extend the decision object) so call sites can pass it back for reconciliation
- [ ] 1.3 Map reconciliation Redis failures to `log.warning` + OTel span event on `agent.budget`; return normally — never raise, never produce a wire frame
- [ ] 1.4 Unit tests (pgmock/miniredis per existing `tests/test_budget.py` patterns): net charge = real after reconcile; hour-straddle reconcile lands on the pre-charge bucket; reconcile Redis failure leaves estimate charged and does not raise; reconcile skipped when `real_tokens` is 0/None (no provider usage)

## 2. Call-site wiring

- [ ] 2.1 Executor `_run_llm`: reconcile right after `accumulate_usage`, using the decision's estimate and hour key from this call's `check_and_increment`
- [ ] 2.2 Executor `_run_react`: reconcile per iteration (each iteration has its own pre-charge decision)
- [ ] 2.3 Executor `_reprompt_for_corrected_call`: reconcile after its `accumulate_usage`
- [ ] 2.4 Planner structured path: add `check_and_increment` before each attempt (retries included) and reconcile after each attempt's usage accrual, including parse-failure attempts; map budget decisions to the existing `budget_exceeded` / `budget_unavailable` termination path
- [ ] 2.5 Planner legacy path: same gate + reconcile pair
- [ ] 2.6 Skip reconcile at sites where the provider returned no usage (net charge stays the estimate), matching the delta spec scenario

## 3. Verification

- [ ] 3.1 Extend `tests/test_executor.py` / `tests/test_planner.py` stubs so scripted budget stubs expose the hour key and record reconcile calls; assert one reconcile per model call with the right (estimate, real) pair
- [ ] 3.2 Add an end-to-end-ish test: full executor node run over a scripted plan asserts the counter's net delta equals the sum of real usage, not calls × 1000
- [ ] 3.3 Run full agent_py suite bare config (`env -u TEST_ASYNC_PG_DSN .venv/bin/python -m pytest -q`) — all green, no new mypy/ruff findings on touched files
- [ ] 3.4 Live check on the side stack: one short chat run, then `redis-cli GET agent:budget:u=<id>:h=<hour>` shows the reconciled value (planner 806 + executor ~50 ballpark, not calls × 1000)
