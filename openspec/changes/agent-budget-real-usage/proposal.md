# Proposal

## Why

The budget guard pre-charges a flat `DEFAULT_ESTIMATED_TOKENS = 1000` per model call while the real per-call cost ranges from ~50 (a short executor answer) to 4000+ (a react iteration carrying RAG context). Short-conversation users accumulate ~800 phantom tokens per step and hit the hourly `budget_exceeded` wall long before spending their real quota; heavy RAG steps undercharge. Now that every call site reports provider-accurate usage (done in the real-token-usage accounting work), the counter can be reconciled to truth instead of estimate accumulation.

## What Changes

- `BudgetGuard` gains a reconciliation operation: after a model call returns, the guard reverses the estimate it pre-charged (`DECRBY` the estimated amount) and charges the provider-reported real usage (`INCRBY` the real amount).
- The three executor call sites (`_run_llm`, each `_run_react` iteration, `_reprompt_for_corrected_call`) and the planner's structured + legacy paths reconcile immediately after their call completes, using the same usage value already accumulated into `AgentState`.
- The pre-charge gate itself is unchanged: checks still happen before every call, on the estimate; the crash window (process dies between pre-charge and reconcile) deliberately over-charges, never under-charges.
- Per-hour bucket keying (`utc_hour_key`, TTL 90 min) is unchanged. Reconciliation that lands after the hour boundary charges the bucket the pre-charge went to.
- Failure of the reconciliation Redis calls is best-effort: logged, never fails the run (the guard's fail-closed contract applies only to the pre-charge check).

## Capabilities

### New Capabilities

(none)

### Modified Capabilities

- `agent-budget-guard-py`: the per-user hourly counter requirement changes from "incremented by the estimated tokens" to "pre-charged by the estimate, then reconciled to the provider-reported real usage after the call completes"; a best-effort reconciliation requirement is added; the cap-enforcement, fail-closed, span, and env-cap requirements are unchanged.

## Impact

- `backend/agent_py/agent/budget.py` — new reconcile method (DECRBY + INCRBY, hour-boundary-safe).
- `backend/agent_py/agent/executor.py`, `backend/agent_py/agent/planner.py` — reconcile call at each of the five model-call sites, right where real usage is already accumulated.
- No wire, API, frontend, or Go changes. `budget_exceeded` semantics, the OTel `agent.budget` span shape, and `AGENT_BUDGET_PER_USER_PER_HOUR` are unchanged.
- Non-goals (candidate future changes): plan-time whole-plan quota pre-check; graceful degradation on `budget_exceeded` (answer from gathered data instead of terminating).
