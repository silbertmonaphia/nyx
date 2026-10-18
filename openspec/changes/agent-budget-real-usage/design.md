# Design

## Context

`BudgetGuard` (`agent/budget.py`) guards an hourly Redis counter (`agent:budget:u={id}:h={YYYYMMDDHH}`, TTL 5400 s on first write) with `check_and_increment(user_id, estimated_tokens, tracer)` — one round trip that pre-charges the estimate and compares against `AGENT_BUDGET_PER_USER_PER_HOUR`. Three executor sites call it before every model call (`_run_llm`, each `_run_react` iteration, `_reprompt_for_corrected_call`); the planner's structured and legacy paths do not gate on the budget at all. Since the real-token-usage accounting change, every call site already computes the provider-reported usage into `AgentState` (`accumulate_usage` / `accumulate_usage_metadata`) immediately after its call returns. `budget.py` already uses `DECRBY` in its rollback path, so the reconciliation primitives exist.

## Goals / Non-Goals

- Goals: hourly counter converges to real per-call spend; no new failure mode for the run; no change to gate semantics, cap config, span shape, or wire frames.
- Non-Goals: plan-time whole-plan quota pre-check; graceful degradation on `budget_exceeded`; per-user cap overrides; tokenizer-based pre-charge refinement. Each is a candidate follow-up change.

## Decisions

### D1: Reconcile inside `BudgetGuard` as one method, called from each call site

`BudgetGuard.reconcile(user_id, estimated_tokens: int, real_tokens: int, tracer) -> None`: `DECRBY` the estimate, then `INCRBY` the real amount, both against the bucket derived from the pre-charge timestamp captured at check time.

Alternative considered — reconcile inside `accumulate_usage` (state layer): rejected, it couples state bookkeeping to the budget backend and drags the guard into every test that touches state.

### D2: Reconcile against the pre-charge's hour, not "now"

`check_and_increment` returns its decision (and effectively the bucket it charged). The call site passes that same hour key context to `reconcile`, so a call that straddles the hour boundary cannot `DECRBY` hour H's estimate into hour H+1 (which would under-charge H and corrupt H+1). `check_and_increment`'s return already carries `tokens_used`; extend it (or the decision object) with the charged hour key so reconcile targets the same key.

Alternative considered — recompute the hour at reconcile time: rejected for the straddle bug above.

### D3: One-way crash window stays, deliberately

If the process dies between pre-charge and reconcile, the estimate stays charged. There is no persistent journal of un-reconciled estimates, and building one (write-ahead log, outbox) is out of proportion for an hourly self-expiring bucket. Over-charge direction only; TTL bounds the damage to ≤90 min past the hour.

### D4: Planner paths gate and reconcile too (scope correction, cheap now)

The structured and legacy planner paths currently consume budget without ever checking it — an uncharged call is an under-charge the reconciliation would otherwise paper over. Give them the same `check_and_increment` + `reconcile` pair: gate before each attempt (including retries), reconcile after each attempt's usage is accrued. The planner-failure path then fails closed like the executor's.

Alternative considered — leave the planner uncharged (out of scope): rejected; it directly contradicts the "counter reflects real spend" goal and the fix is a few lines at sites this change already touches.

### D5: Estimate constant stays 1000

The pre-charge estimate is a gate-floor, not an accounting figure, once reconciliation lands. Refining it (tokenizer pre-count) has weak marginal value (see the conversation that spawned this change: the dominant cost is re-sent loop context, unknowable pre-call for react iterations). Keep `DEFAULT_ESTIMATED_TOKENS` as is; revisit only if real-world reconciled data shows the floor is too low to bound a single call.

### D6: Reconciliation Redis errors — log, count, continue

Wrap the two Redis ops in the guard's existing error taxonomy but map failures to `log.warning` (+ the run's OTel span as an event) and return normally. Never emit a wire frame for a reconciliation failure. The spec's "errs toward over-charge" scenario is the contract.

## Risks / Trade-offs

- **Retroactive overrun**: a 3500-real call passes a gate calibrated at 1000 and pushes the counter past the cap after the fact. Accepted: the alternative (tokenizer pre-count of the whole outgoing message list) is precise for prompt tokens but still blind to completions and future react iterations; the gate's job is bounding runaway loops, and post-hoc truth makes the hourly cap meaningful again.
- **Double-spend on react resume**: a resumed run re-enters a react iteration whose pre-charge survived the crash. The langGraph resume path already skips completed nodes; the residual window (pre-charge executed, node checkpoint not written) over-charges by one estimate, bounded as in D3.
- **Metrics discontinuity**: `agent_tokens_total` (prometheus) already reports real usage; the budget counter now converges to it. Dashboards comparing the two stop diverging — any historical alerting keyed to the old inflation needs a cap-tuning pass after rollout.
