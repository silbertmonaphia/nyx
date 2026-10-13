# Agent Observability Specification

## Purpose

Makes agent runs, steps, and tool calls observable through OTel and Prometheus, and enforces per-user token budgets so the agent layer cannot exceed cost caps. **Implementation moved to Python LangGraph** (see OpenSpec change `agent-langgraph`); the OTel span names, Prometheus counter names, and Redis budget key contract stay identical so dashboards and alerts continue to work.

## Requirements

### Requirement: OTel spans cover runs and steps

Every agent run SHALL produce one `agent.run` span carrying `run_id`, `user_id`, `plan_size`, and `total_tokens`. Each step SHALL produce one `agent.step.{plan,tool,react,llm}` span carrying `step_id`, `kind`, `name` (when applicable), `iteration` (when applicable), and token counts. Tool-call timings SHALL be recorded as span attributes on the relevant `agent.step.tool` span.

#### Scenario: Run trace shows full plan structure

- **WHEN** an authenticated user runs the agent with a 4-step plan that includes one tool call and one nested ReAct
- **THEN** the resulting trace in Jaeger shows `agent.run` as the root with four `agent.step.*` children; the `react` step has an iteration attribute > 1; the `tool` step has a tool-call timing attribute

#### Scenario: Provider latency visible per step

- **WHEN** a tool call takes longer than the model call it precedes
- **THEN** the trace visually shows the tool span longer than the surrounding model spans, with the timing attribute reading the actual measured duration

### Requirement: Prometheus counters track agent activity

The following counters MUST exist and MUST be incremented on the corresponding path:
- `agent_runs_total{kind, status}` — incremented on every run completion or terminal failure
- `agent_steps_total{kind, status}` — incremented on every step completion or failure
- `agent_tool_calls_total{tool, ok}` — incremented on every tool dispatch outcome
- `agent_tokens_total{role, model}` — incremented with every token delta and usage frame, tagged by `role` (planner / executor / rag / react) and `model`
- `agent_tool_retries_total{tool, reason}` — incremented once per retry attempt (not on first-try success); `tool` is the registry tool name, `reason` is one of `tool_arg_invalid` (registry rejected the call's arguments) or `tool_execution_transient` (network timeout, 5xx, 408, 429)
- `agent_runs_swept_total` — incremented by the stale-run sweeper with the number of `running` rows reaped in each sweep iteration (see `agent-run-sweeper`); zero-reap iterations do not increment it

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

### Requirement: Per-user token budget guard fails closed

The agent loop MUST consult a Redis-backed per-user hourly counter before each model call. When the counter exceeds `AGENT_BUDGET_PER_USER_PER_HOUR` (or default), the agent MUST refuse the call and emit `error {code:"budget_exceeded"}`. When the counter backend is unreachable, the agent MUST refuse the call and emit `error {code:"budget_unavailable"}` — never `return`s the call failing open.

#### Scenario: Hourly cap enforced across runs

- **WHEN** a user has already consumed `AGENT_BUDGET_PER_USER_PER_HOUR` tokens this hour in a prior run
- **THEN** the next run's first model call is rejected with `budget_exceeded`; the run does not produce a plan

#### Scenario: Counter unavailable

- **WHEN** the Redis counter backend is unreachable at budget check time
- **THEN** the agent emits `error {code:"budget_unavailable"}` and no model call is issued; the run terminates

### Requirement: Budget check is its own OTel span

Every budget check SHALL emit an `agent.budget` span with attributes `user_id`, `tokens_used`, `tokens_remaining`, and `decision` (`allow` / `exceeded` / `unavailable`). Budget exhaustion MUST be correlatable to the run trace so operators can answer "why did this run terminate?" from the trace alone.

#### Scenario: Budget exhaustion traceable from run

- **WHEN** a run terminates with `budget_exceeded`
- **THEN** the trace shows the `agent.budget` span with `decision="exceeded"` as a child of the `agent.run` span, in time order before any `error` event

### Requirement: Agent is default-off

The agent loop MUST NOT execute unless `AGENT_ENABLED=true`. When `AGENT_ENABLED=false`, the chat handler falls back to today's single-turn behaviour. The default-off state MUST be the default in shipped images, env files, and CI test setup.

#### Scenario: Default-off in CI

- **WHEN** CI runs the backend test suite without setting `AGENT_ENABLED`
- **THEN** no agent-specific paths execute, no agent spans are emitted, and no budget counter is consulted — single-turn fallback paths run instead
