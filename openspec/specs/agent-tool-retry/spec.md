# Agent Tool Retry Specification

## Purpose

Bounded, observable retry-and-recover for tool-dispatch failures in the Python agent executor. Covers two failure classes the planner-side retry in `agent-planner-structured` deliberately left out: model-emitted tool calls that the registry rejects, and transient errors from the tool's remote call (network, MCP / RAG bridge timeouts). The retry budget is per-step and per-tool; a single bad tool call cannot burn the budget for the run.

## Requirements

### Requirement: Registry-rejected tool calls are retried with validator feedback

When a tool dispatch fails because the registry rejected the call's arguments (Pydantic validation, missing required field, wrong type), the executor SHALL re-invoke the model up to `PYTHON_AGENT_TOOL_RETRY_MAX` times total, appending the validator's error message to the re-prompt's final user message under a `PREVIOUS_ATTEMPT_FAILED` block so the model can self-correct on the next attempt. The retry budget MUST be per-step and per-tool — a retry exhausted on one tool does not consume budget for sibling tools in the same step, and a retry exhausted on one step does not consume budget for later steps.

#### Scenario: Validator rejection is retried once

- **WHEN** the executor dispatches a tool and the registry returns a typed validation error
- **THEN** the executor re-invokes the model with the validator's error appended under `PREVIOUS_ATTEMPT_FAILED`; the retry attempt's `tool_result` frame carries `retry_count: 1`

#### Scenario: Persistent validation failure terminates the step

- **WHEN** the model emits a tool call that the registry rejects on every attempt up to `PYTHON_AGENT_TOOL_RETRY_MAX`
- **THEN** the step terminates with `step_failed` and a final `tool_result` frame whose `error` field carries the last validator's message (mapped through the safe-error funnel — internal error text MUST NOT appear on the wire); the run continues to the next step

#### Scenario: Retry budget is per-step, not per-run

- **WHEN** step A exhausts its tool-arg retry budget
- **THEN** step B starts with a fresh budget; exhaustion in A MUST NOT prevent retries in B

#### Scenario: Retry cap is configurable

- **WHEN** `PYTHON_AGENT_TOOL_RETRY_MAX=N` is set (default 2, must be ≥ 0)
- **THEN** the executor retries up to N times before terminating the step; values below 0 are rejected at service startup

### Requirement: Transient tool-execution failures are retried with bounded backoff

When a tool dispatch fails with a transient error (network timeout, connection reset, 5xx from the MCP or RAG bridge, 408 / 429 from any HTTP-backed tool), the executor SHALL retry with exponential backoff (base 250 ms, factor 2, jittered) bounded by `PYTHON_AGENT_TOOL_RETRY_MAX` attempts and a hard ceiling of 5 s between attempts. Non-retryable errors (ownership / scope failure, authentication failure, 4xx other than 408 / 429) MUST fail the step immediately with no retry. Registry validation rejections are NOT in this class — they follow the tool-arg retry path above. The classification of retryable vs non-retryable lives in the executor, not the registry; the registry only returns the typed error class.

#### Scenario: Network timeout is retried

- **WHEN** the tool call raises a network timeout against the MCP / RAG bridge
- **THEN** the executor retries with exponential backoff up to `PYTHON_AGENT_TOOL_RETRY_MAX` total attempts; each retry's `tool_result` frame carries the attempt number

#### Scenario: Validation error is not retried as transient

- **WHEN** the tool call returns a typed validation error
- **THEN** the executor routes the failure to the validator-feedback retry path, not the transient-retry path — the two paths MUST NOT both fire on the same error

#### Scenario: 4xx other than 408/429 fails the step immediately

- **WHEN** the tool call returns HTTP 400 / 401 / 403 / 404 from the bridge
- **THEN** the executor fails the step immediately with the mapped error — no retry, no `agent_tool_retries_total` increment

#### Scenario: Backoff is bounded

- **WHEN** the retry budget is exhausted on transient failures
- **THEN** the cumulative wall-clock time across the step's retries is bounded by `PYTHON_AGENT_TOOL_RETRY_MAX` × 5 s (the per-attempt ceiling); the step's overall timeout is enforced by the existing `AGENT_STEP_TIMEOUT` semantics, not by the retry loop

### Requirement: Tool retries are observable

Every retry attempt — of either class — SHALL increment the Prometheus counter `agent_tool_retries_total` exactly once. The counter carries labels `tool` (registry tool name, e.g. `list_feeds`) and `reason` (one of `tool_arg_invalid`, `tool_execution_transient`). A successful first attempt MUST NOT increment the counter. The counter is process-local and MUST survive a clean restart; it does not need to survive a crash.

#### Scenario: Successful first attempt does not increment

- **WHEN** the executor dispatches a tool and the first attempt succeeds
- **THEN** the `tool_result` frame carries `retry_count: 0` and `agent_tool_retries_total` is not incremented for that call

#### Scenario: Each retry increments the counter once

- **WHEN** the executor retries a tool call (for either reason)
- **THEN** each retry attempt increments `agent_tool_retries_total{tool=<name>, reason=<reason>}` by 1; a successful retry followed by a successful next attempt increments the counter only for the retry attempts, not for the eventual success

#### Scenario: Retries are correlated with the run trace

- **WHEN** a retry occurs
- **THEN** the surrounding `agent.step.tool` OTel span carries attributes `retry_count` and `retry_reason`; an operator navigating from `agent_tool_retries_total` to the trace sees the failed attempt and the recovery attempt in the same trace

### Requirement: Tool-arg retry does not re-emit the original tool_call

When the executor re-invokes the model after a registry rejection, the new model call MUST NOT carry the failed tool call as a prior assistant message verbatim — the model is being asked to *correct* the call, not re-affirm it. The re-prompt structure is: original messages, the assistant message that emitted the bad call (so the model sees its own attempt), the rejected `tool_result` frame with the validator's error, and a final user message containing `PREVIOUS_ATTEMPT_FAILED: <error>` plus the registry's argument schema for the tool.

#### Scenario: Re-prompt includes the rejected tool call and its validator error

- **WHEN** the executor retries after a registry rejection
- **THEN** the new model call's message list ends with the assistant message that emitted the bad call, the matching tool_result carrying the validator's error, and a final user message that quotes the error and the tool's argument schema

#### Scenario: Re-prompt does not duplicate the original user request

- **WHEN** the executor retries
- **THEN** the original user request is carried over from the prior model call (it appears in the assistant's prior turn or earlier in the message list) — it is NOT re-quoted in the new final user message

### Requirement: Idempotency key is stable across retries and resume

For every registry tool call whose served input schema declares an `idempotency_key` argument, the agent SHALL derive exactly one key per intent and attach it to every attempt of that intent. The key MUST be deterministic over the run id, turn id, step id, tool name, and canonicalised arguments. The turn id MUST be minted per turn and carried in the checkpointed graph state, so two identical turns in one chat session whose run id is the shared session id derive different keys, while a resumed run reproduces the original turn's key. Transient retries and re-prompts that preserve the tool name and arguments MUST reuse the same key; a corrected re-prompt that changes either MUST derive a new key. The per-attempt `call_id` used by `tool_call` / `tool_result` frames is independent of the key and MAY differ between attempts. The key MUST be derived before the approval gate so that a retry after an approval decision reuses it.

#### Scenario: Transient retry reuses the key

- **WHEN** a mutating tool call fails with a transient error and the executor retries with unchanged arguments
- **THEN** every attempt carries the same `idempotency_key`, while each attempt's `call_id` is distinct

#### Scenario: Corrected arguments form a new intent

- **WHEN** a registry rejection produces a corrected re-prompt that changes the tool name or arguments
- **THEN** the next attempt carries a different `idempotency_key`

#### Scenario: Resume reproduces the key

- **WHEN** a run resumes after a crash and re-executes a plan step whose tool and arguments are unchanged
- **THEN** the dispatch carries the same `idempotency_key` the original execution used, derived from the checkpointed turn id

#### Scenario: A new turn in the same session gets a new key

- **WHEN** a later turn of the same chat session repeats an earlier turn's tool call with identical arguments and step id
- **THEN** the dispatch carries a different `idempotency_key` because the turn id differs, so the mutation executes rather than replaying the earlier turn's response

#### Scenario: Approval wait does not change the key

- **WHEN** a mutating call pauses for user confirmation and is dispatched after an approve decision, or is retried after a crash following that decision
- **THEN** the dispatched attempt uses the key derived before the pause

### Requirement: Idempotency keys are derived only for declaring tools

The agent SHALL add an `idempotency_key` argument only when the served input schema declares that property. Calls to tools whose schemas do not declare it — including read-only tools — MUST be dispatched with unchanged arguments. A value supplied by the model for that argument MUST be overridden by the derived key, never trusted.

#### Scenario: Declaring tool receives the derived key

- **WHEN** the model calls `create_feed`, whose served schema declares `idempotency_key`
- **THEN** the dispatched arguments carry the derived key even if the model omitted or supplied a different value

#### Scenario: Non-declaring tool is unchanged

- **WHEN** the model calls `list_feeds`, whose served schema does not declare `idempotency_key`
- **THEN** the dispatched arguments are exactly the model's arguments, with no added field
