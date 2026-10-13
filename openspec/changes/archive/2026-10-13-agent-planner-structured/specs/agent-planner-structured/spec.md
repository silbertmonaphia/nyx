# Spec Delta

## Purpose

Replaces free-text JSON parsing in the Python agent's planner with provider-side structured output so plan emission is reliable at every model tier, with a bounded retry on the residual validation failures (schema drift, truncation) instead of terminating the run.

## ADDED Requirements

### Requirement: Plan obtained via provider structured output

The planner SHALL obtain the `Plan` object via a provider structured-output binding (Pydantic-typed response, JSON-schema response format, or provider tool-call wire) and SHALL NOT run `json.loads` on raw free-text model output as part of the primary path. When the binding is disabled by configuration, the planner MAY fall back to the legacy free-text path; the primary path MUST go through the structured binding.

#### Scenario: Structured binding returns parsed Plan on first try

- **WHEN** the planner invokes the model with `Plan` as the response schema and the model returns a schema-conformant response
- **THEN** the planner receives a Pydantic `Plan` instance derived from schema-constrained provider output — no fence-stripping and no `json.loads` on unconstrained free-text

#### Scenario: Fence-stripping code is unreachable on the primary path

- **WHEN** the structured binding is enabled
- **THEN** no code path on the primary planner flow executes the legacy fence-stripping or `json.loads` logic — that code is either deleted or guarded behind a configuration flag whose default places it on a non-primary branch

### Requirement: Plan validated through Pydantic

The planner SHALL validate every emitted `Plan` against the Pydantic model declared in `agent_py/agent/types.py`. Validation failures (missing fields, wrong types, malformed `PlanStep`) MUST surface as typed errors the retry policy can consume, and the planner MUST emit the existing `error{code:"plan_invalid"}` SSE frame only after the retry budget is exhausted.

#### Scenario: Pydantic-rejected plan triggers retry, not run termination

- **WHEN** the structured binding produces output that fails Pydantic validation on the first attempt
- **THEN** the planner treats the failure as a retryable error rather than terminating the run

#### Scenario: Persistent Pydantic failure terminates run

- **WHEN** the structured binding produces output that fails Pydantic validation on every retry up to the configured cap
- **THEN** the planner emits `error{code:"plan_invalid"}` followed by `done`, terminating the run before any step executes

### Requirement: Bounded retry with validator error feedback

The planner SHALL retry a failed plan emission up to `PYTHON_AGENT_PLANNER_RETRY_MAX` times (default 1). On each retry the planner SHALL append the validator's or parser's error message to the final user message of the re-prompt (under a `PREVIOUS_ATTEMPT_FAILED` block) so the model can self-correct on the next attempt. The retry counter MUST reset per run and MUST NOT be persisted.

#### Scenario: Retry cap defaults to one

- **WHEN** the planner is invoked with no override of the planner-retry setting
- **THEN** a single validation failure produces one re-prompt; a second consecutive validation failure terminates the run with `error{code:"plan_invalid"}`

#### Scenario: Retry prompt contains the validation error

- **WHEN** the planner retries after a Pydantic failure
- **THEN** the re-prompt's final user message includes the validator's error message so the model can target its correction

#### Scenario: Retry cap is configurable

- **WHEN** `PYTHON_AGENT_PLANNER_RETRY_MAX=N` is set
- **THEN** the planner retries up to N times before terminating; values below 0 are rejected at startup

### Requirement: Cross-provider parity for plan emission

The planner SHALL emit an equivalent `Plan` against OpenAI-shaped and vLLM-shaped provider responses. With the `response_format` json_schema binding, both providers return the plan as schema-constrained JSON content; the integration test suite MUST replay a canned schema-conformant response through both providers' response envelopes (real request/response bytes, via a mocked transport driving the actual client) and assert identical parsed `Plan` shape, identical step ordering, and identical field values. A response that carries `tool_calls` instead of content (the historical vLLM tool-call wire divergence) MUST be treated as a retryable conformance failure — re-prompted with feedback and terminated with `error{code:"plan_invalid"}` only if the divergence persists — never a crash of the graph node and never a silent drop.

#### Scenario: OpenAI and vLLM replay the same canned plan

- **WHEN** a test replays a canned schema-conformant plan response through an OpenAI-shaped response envelope and the same plan content through a vLLM-shaped response envelope (vendor-extra fields tolerated)
- **THEN** both replays yield identical `Plan` instances with identical `steps`, identical `id`s, and identical field values

#### Scenario: tool_calls-only response is a retryable divergence

- **WHEN** a provider responds to the structured binding with `tool_calls` and no content
- **THEN** the planner treats it as a retryable conformance failure, re-prompts with the parser's feedback under `PREVIOUS_ATTEMPT_FAILED`, and either recovers on a later attempt or terminates with `error{code:"plan_invalid"}` if the divergence persists

#### Scenario: Provider divergence surfaces in the test, not in production

- **WHEN** the provider wire shape changes (e.g. a response envelope field is added, or content disappears in favor of tool calls)
- **THEN** the cross-provider test fails loudly or exercises the retry path; the executor and run-time frame contract remain unchanged

### Requirement: Parse-failure observability counter

The agent SHALL expose a Prometheus counter `agent_planner_parse_failures_total` with label `retry` whose values are `ok` (recovered on retry) and `exhausted` (terminated with `plan_invalid`). Each run that experiences a Pydantic validation failure SHALL increment the counter exactly once: `ok` when a later attempt succeeds within the retry budget, `exhausted` when the run terminates with `plan_invalid`. A run with no validation failure SHALL NOT increment the counter. Terminal `plan_too_large` rejections SHALL NOT increment it.

#### Scenario: Counter tracks validation failures

- **WHEN** the planner experiences one recoverable validation failure followed by a successful plan emission
- **THEN** `agent_planner_parse_failures_total{retry="ok"}` increments by exactly one and `agent_planner_parse_failures_total{retry="exhausted"}` does not increment for that run

#### Scenario: Counter tracks persistent failures

- **WHEN** the planner exhausts its retry budget on a single run
- **THEN** `agent_planner_parse_failures_total{retry="exhausted"}` increments by exactly one for that run
