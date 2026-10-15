# Spec Delta

## Purpose

Caps per-user hourly token spend inside the Python orchestrator so a runaway loop, a stuck react iteration, or a misconfigured cap cannot exhaust the project's LLM budget. Fails closed when Redis is unavailable rather than silently allowing the call to proceed.

## ADDED Requirements

### Requirement: Per-user hourly counter in Redis

The orchestrator SHALL maintain a per-user hourly token counter at the Redis key `agent:budget:u={user_id}:h={utc_hour}` where `{utc_hour}` is the UTC `YYYYMMDDHH` formatted integer. The counter SHALL be incremented (`INCRBY`) by the estimated tokens consumed before each `LLM.chat` call inside the loop, with TTL 90 minutes set on first increment.

#### Scenario: Counter increments per model call

- **WHEN** the orchestrator is about to make a model call consuming an estimated `N` tokens
- **THEN** it does `INCRBY agent:budget:u={user_id}:h={utc_hour} N` and (if the counter was newly created) `EXPIRE 5400`

#### Scenario: Counter resets at the UTC hour boundary

- **WHEN** the hour rolls over to the next UTC hour
- **THEN** the orchestrator computes a new key `agent:budget:u={user_id}:h={utc_hour_plus_one}` and the previous key's TTL expires

### Requirement: Budget guard enforces the cap

The orchestrator SHALL refuse to invoke the model when the current counter value would exceed `AGENT_BUDGET_PER_USER_PER_HOUR` (default 200000). Refusal SHALL emit `error{code:"budget_exceeded"}` followed by `done` + `terminator`. The orchestrator SHALL NOT silently fall back to "skip the check" or "warn and proceed".

#### Scenario: Hourly cap enforced across runs

- **WHEN** a user has already consumed `AGENT_BUDGET_PER_USER_PER_HOUR` tokens this hour in a prior run
- **THEN** the next run's first model call is rejected with `budget_exceeded`; the run does not produce a plan

#### Scenario: Cap hit mid-run

- **WHEN** the run has consumed `AGENT_BUDGET_PER_USER_PER_HOUR` tokens and is about to make another model call
- **THEN** the orchestrator emits `error{code:"budget_exceeded"}` and no further model calls execute

### Requirement: Budget guard fails closed on Redis unavailability

The orchestrator SHALL treat any Redis error (connection refused, command timeout, command error) as "counter backend unavailable". The guard MUST refuse the model call and emit `error{code:"budget_unavailable"}` rather than fail open. The orchestrator SHALL NOT retry the counter call indefinitely; one attempt per check.

#### Scenario: Redis down on startup

- **WHEN** the first counter `INCRBY` returns a connection error
- **THEN** the orchestrator emits `error{code:"budget_unavailable"}` and no model call is issued

#### Scenario: Redis down mid-run

- **WHEN** a counter check fails mid-run after a prior check succeeded
- **THEN** the orchestrator emits `error{code:"budget_unavailable"}` and the run terminates

### Requirement: agent.budget OTel span on every check

Every budget check SHALL emit an OTel span named `agent.budget` with attributes `user_id` (int), `tokens_used` (int — the value the counter reached after `INCRBY`), `tokens_remaining` (int — `cap - tokens_used`, may be zero or negative), and `decision` (`allow` | `exceeded` | `unavailable`). Budget exhaustion MUST be correlatable to the parent `agent.run` span so operators can answer "why did this run terminate?" from the trace alone.

#### Scenario: Allow path span

- **WHEN** the budget check passes
- **THEN** the `agent.budget` span has `decision="allow"`, `tokens_used=<N>`, `tokens_remaining=<cap-N>`

#### Scenario: Exceeded path span

- **WHEN** the budget check exceeds
- **THEN** the `agent.budget` span has `decision="exceeded"`, `tokens_used=<cap-or-greater>`, `tokens_remaining=<0-or-negative>`, parented under `agent.run`

#### Scenario: Unavailable path span

- **WHEN** the Redis check fails
- **THEN** the `agent.budget` span has `decision="unavailable"` with the underlying error attached as a span event (NOT in the response wire)

### Requirement: Cap is environment-driven

The cap SHALL be read from `AGENT_BUDGET_PER_USER_PER_HOUR` (default 200000). The cap is not in code, not in a DB table, and not a per-user override. Per-user overrides land in a follow-up change (per `FUTURE_AGENT.md` §P1.2).

#### Scenario: Default cap applied when env unset

- **WHEN** the orchestrator starts with `AGENT_BUDGET_PER_USER_PER_HOUR` unset
- **THEN** the cap defaults to 200000 tokens per user per hour