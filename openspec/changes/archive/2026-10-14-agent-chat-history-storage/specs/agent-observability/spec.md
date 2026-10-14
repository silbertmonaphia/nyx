# Spec Delta

## Purpose

(see existing capability)

## MODIFIED Requirements

### Requirement: Prometheus counters track agent activity

The following counters MUST exist and MUST be incremented on the corresponding path:
- `agent_runs_total{kind, status}` — incremented on every run completion or terminal failure
- `agent_steps_total{kind, status}` — incremented on every step completion or failure
- `agent_tool_calls_total{tool, ok}` — incremented on every tool dispatch outcome
- `agent_tokens_total{role, model}` — incremented with every token delta and usage frame, tagged by `role` (planner / executor / rag / react) and `model`
- `agent_tool_retries_total{tool, reason}` — incremented once per retry attempt (not on first-try success); `tool` is the registry tool name, `reason` is one of `tool_arg_invalid` (registry rejected the call's arguments) or `tool_execution_transient` (network timeout, 5xx, 408, 429)
- `agent_runs_swept_total` — incremented by the stale-run sweeper with the number of `running` rows reaped in each sweep iteration (see `agent-run-sweeper`); zero-reap iterations do not increment it
- **`agent_chat_retention_deleted_total`** — incremented by the number of `chat_sessions` rows hard-deleted by the retention cron (soft-deletes are NOT counted); incremented once per iteration, by `chat_deleted_count`

The following OTel spans MUST be produced:
- `agent.run` (run_id, user_id, plan_size, total_tokens) — one per run
- `agent.step.{plan,tool,react,llm}` (step_id, kind, name?, iteration?, token counts) — one per step
- `agent.run.retention` — one per run-retention iteration (deleted_count attribute)
- **`agent.chat.retention`** — one per chat-retention iteration (chat_deleted_count attribute); emitted after `agent.run.retention` on the same tick
- `agent.run.sweep` — one per stale-run-sweeper iteration (reaped_count attribute)

#### Scenario: Planner vs executor token split is visible

- **WHEN** a planner-only call uses `LLM_PLANNER_MODEL` and the executor uses the chat model
- **THEN** the `agent_tokens_total{kind="planner"}` series reports tokens under the planner model label and `agent_tokens_total{kind="executor"}` reports under the executor label — operators can see the split at a glance

#### Scenario: Tool-retry counter increments on each retry attempt

- **WHEN** the executor retries a tool dispatch for any reason
- **THEN** `agent_tool_retries_total{tool=<name>, reason=<reason>}` increments by exactly 1 per retry attempt; a successful first attempt does not increment it; a successful retry followed by a successful next attempt increments it only for the retry attempts, not the eventual success

#### Scenario: Tool-retry counter labels are bounded

- **WHEN** the counter is sampled
- **THEN** the `tool` label set is the registered tool set (no free-form strings from model output); the `reason` label set is the closed enumeration `tool_arg_invalid` or `tool_execution_transient` — no other values appear

#### Scenario: Reaped stale runs are visible

- **WHEN** the sweeper reaps N stale `running` rows
- **THEN** `agent_runs_swept_total` increases by N on the `:9001/metrics` endpoint, and an operator can graph reaped-run volume over time

#### Scenario: Retention counter increments per iteration

- **WHEN** the retention cron iterates and hard-deletes N chat sessions
- **THEN** `agent_chat_retention_deleted_total` increments by exactly N on that iteration
- **AND** `agent.run.retention` and `agent.chat.retention` both appear in Jaeger for that tick

#### Scenario: Soft-deletes are not counted

- **WHEN** the retention cron iterates and only soft-deletes (no hard-deletes)
- **THEN** `agent_chat_retention_deleted_total` does NOT increment
- **AND** the `agent.chat.retention` span is still emitted (visibility on soft-delete traffic)
