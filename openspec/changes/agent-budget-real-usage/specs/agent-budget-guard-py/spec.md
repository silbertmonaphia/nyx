# agent-budget-guard-py Delta

## MODIFIED Requirements

### Requirement: Per-user hourly counter in Redis

The orchestrator SHALL maintain a per-user hourly token counter at the Redis key `agent:budget:u={user_id}:h={utc_hour}` where `{utc_hour}` is the UTC `YYYYMMDDHH` formatted integer. Before each model call the orchestrator SHALL pre-charge the counter (`INCRBY`) by the estimated tokens for that call, with TTL 90 minutes set on first increment. After the model call completes, the orchestrator SHALL reconcile the counter to the provider-reported real usage: it SHALL decrement (`DECRBY`) by the estimated amount it pre-charged and increment (`INCRBY`) by the real amount consumed, in one best-effort pass. The pre-charge-and-check gate remains the sole authority for refusing a call; reconciliation never re-opens a refused call.

#### Scenario: Counter increments per model call

- **WHEN** the orchestrator is about to make a model call consuming an estimated `N` tokens
- **THEN** it does `INCRBY agent:budget:u={user_id}:h={utc_hour} N` and (if the counter was newly created) `EXPIRE 5400`

#### Scenario: Counter resets at the UTC hour boundary

- **WHEN** the hour rolls over to the next UTC hour
- **THEN** the orchestrator computes a new key `agent:budget:u={user_id}:h={utc_hour_plus_one}` and the previous key's TTL expires

#### Scenario: Reconciliation after a cheaper-than-estimated call

- **WHEN** a model call pre-charged `N=1000` and the provider reports `R=200` real tokens
- **THEN** the counter is decremented by `1000` and incremented by `200`, so the net charge for the call is `200`; the next gate check sees the reconciled value

#### Scenario: Reconciliation after a costlier-than-estimated call

- **WHEN** a model call pre-charged `N=1000` and the provider reports `R=3500` real tokens
- **THEN** the counter is decremented by `1000` and incremented by `3500`, so the net charge for the call is `3500`; the overrun applies after the fact and does not retroactively refuse the call

#### Scenario: Reconciliation keeps the pre-charged hour's bucket

- **WHEN** a call pre-charges in hour `H` and its reconciliation executes after the boundary in hour `H+1`
- **THEN** both the `DECRBY` of the estimate and the `INCRBY` of the real usage land on the `H` bucket the pre-charge went to, leaving `H+1` untouched by that call

#### Scenario: Provider reports no usage

- **WHEN** a model call returns no provider-reported usage (a gateway strips it, or a stub omits it)
- **THEN** reconciliation is skipped for that call, the estimate stays charged, and the run continues; the net charge is the estimate, not an error

## ADDED Requirements

### Requirement: Reconciliation is best-effort and never fails the run

The orchestrator SHALL treat any Redis error during reconciliation (connection refused, command timeout, command error) as non-fatal: the error SHALL be logged and the run SHALL continue. The fail-closed contract (`budget_unavailable`) applies only to the pre-charge check, not to reconciliation. A lost reconciliation leaves the estimate charged — the counter errs toward over-charging, never under-charging.

#### Scenario: Redis degrades between pre-charge and reconcile

- **WHEN** the pre-charge `INCRBY` succeeds and the reconciliation `DECRBY` fails with a connection error
- **THEN** the run continues normally, the estimate remains charged, and the error is logged with the run id; no `budget_unavailable` frame is emitted for the reconciliation failure

#### Scenario: Process dies between pre-charge and reconcile

- **WHEN** the orchestrator crashes after pre-charging but before reconciling
- **THEN** the counter retains the estimate for that call (over-charge direction); there is no persistent record of the un-reconciled estimate and no under-charge path exists
