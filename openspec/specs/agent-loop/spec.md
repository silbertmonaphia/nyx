# Agent Loop Specification

## Purpose

Replaces single-turn chat with a Plan-and-Execute agent loop that streams per-step progress to the client while preserving the existing chat SSE wire contract. **The implementation moved to Python LangGraph** (see OpenSpec change `agent-langgraph`); this spec stays as the wire contract the chat handler and frontend observe.

## Requirements

### Requirement: Chat delegates to a Plan-and-Execute agent loop

`POST /api/chat` SHALL delegate request handling to an agent service that runs a Plan-and-Execute loop. The agent service produces a step plan and executes it sequentially. RAG context retrieval is one of the step kinds in the plan, not a pre-LLM prompt injection.

#### Scenario: Agent enabled

- **WHEN** `AGENT_ENABLED=true` and the request is authenticated
- **THEN** the chat handler invokes the agent service, which produces a plan, executes each step in order, and streams the result

#### Scenario: Agent disabled — single-turn fallback

- **WHEN** `AGENT_ENABLED=false`
- **THEN** the chat handler falls back to today's single-turn behaviour: optional RAG pre-injection, one model call, content stream. The wire contract is unchanged

#### Scenario: RAG runs as a step, not as pre-injection

- **WHEN** the request enables RAG and the agent is enabled
- **THEN** RAG retrieval appears in the streamed step trace as a distinct step, not as a hidden prompt prepending before the model's first frame

### Requirement: Plan and steps have stable identifiers

Each step in the emitted plan SHALL carry a stable, unique identifier within that run. Every per-step SSE frame SHALL carry the step's identifier. The client MUST be able to correlate `step_start`, `tool_call`, `tool_result`, and `step_end` frames for the same step purely from the identifier, without relying on event order.

#### Scenario: Identifiers survive reordering

- **WHEN** frames for two steps arrive interleaved due to async tool execution
- **THEN** a client that filters by identifier reconstructs each step's events in the correct order

#### Scenario: Identifiers are unique within a run

- **WHEN** the same tool name appears in two different steps of one plan
- **THEN** the step identifiers differ and the client can render each step's tool call separately

### Requirement: Step kinds cover the agent surface

The agent loop SHALL support at least these step kinds: `rag` (vector retrieval over feeds), `tool` (registry dispatch), `llm` (single model call), and `react` (nested model-tool-model loop with iteration cap). The planner MAY emit any combination of these kinds.

#### Scenario: Planner emits a mixed plan

- **WHEN** the planner produces steps of kinds `[rag, tool, llm, tool]`
- **THEN** the executor runs them in that order, each emits its own `step_start`/`step_end` pair, and tools dispatched match the registry

#### Scenario: Nested ReAct caps iterations

- **WHEN** a step of kind `react` runs and the model emits tool calls
- **THEN** the loop iterates model → tool → model until either the model emits a final answer OR the configured iteration cap is hit; exceeding the cap emits `step_failed` with reason `react_max_iterations`

### Requirement: Step execution is bounded

The agent SHALL enforce `AGENT_PLAN_MAX_STEPS` on every run. A plan exceeding the cap MUST be rejected before execution with a typed `plan_too_large` error surfaced over the `error` SSE event. The agent SHALL NOT silently truncate plans.

#### Scenario: Oversized plan rejected

- **WHEN** the planner returns a plan with more steps than `AGENT_PLAN_MAX_STEPS`
- **THEN** the agent emits `error {code:"plan_too_large"}` and no steps execute

#### Scenario: ReAct iteration cap enforced

- **WHEN** a `react` step exceeds `AGENT_STEP_REACT_MAX_ITERATIONS`
- **THEN** the step terminates with `step_failed {reason:"react_max_iterations"}` and the run terminates with `error {code:"react_iterations_exceeded"}`

### Requirement: Per-step SSE frames preserve the existing chat wire contract

The agent loop SHALL emit SSE frames in this order over a run: `plan` (once, before the first step), then for each step `step_start`, zero or more `tool_call` / `tool_result` frames, then `step_end`. The existing `delta`, `note`, `terminator`, and final `done` frames MUST continue to be emitted with their existing shape; only `error` gains new `code` values (`plan_too_large`, `react_iterations_exceeded`, `budget_exceeded`, `plan_invalid`).

#### Scenario: Existing client sees unchanged wire contract for plan_too_large

- **WHEN** an oversized plan is rejected
- **THEN** an old client that only parses `delta` / `done` / `note` / `error` / `terminator` still sees a final `terminator` and `done`; the `error` frame carries the new `code` but its top-level shape is unchanged

#### Scenario: New client renders intermediate steps

- **WHEN** a new client parses `step_start` / `tool_call` / `tool_result` / `step_end` / `plan`
- **THEN** it renders each step's progress as it streams, and the user sees tool names appear before the final reply

### Requirement: Budget guard terminates runs that exhaust per-user quota

Before each model call inside the loop, the agent SHALL consult a per-user hourly token counter and, when the cap is reached, terminate the run with a `budget_exceeded` error. The counter MUST be Redis-backed (or equivalent shared store) so quota is enforced across backend replicas.

#### Scenario: Budget exhaustion mid-run

- **WHEN** the run has already consumed `AGENT_BUDGET_PER_USER_PER_HOUR` tokens and is about to make another model call
- **THEN** the agent emits `error {code:"budget_exceeded"}`, no further model calls execute, and any steps after the current one do not start

#### Scenario: Budget guard fails closed on counter unavailability

- **WHEN** the counter backend is unreachable
- **THEN** the agent refuses the model call (does not fail open) and emits `error {code:"budget_unavailable"}`

### Requirement: Authenticated callers only

The agent loop SHALL refuse to execute for any caller without a valid Bearer access token. The chat handler's existing auth middleware covers this when `AGENT_ENABLED=true`; the agent service MUST also assert that a user identifier is present on the context at the registry call boundary.

#### Scenario: Unauthenticated request rejected

- **WHEN** a chat request reaches the agent without a valid Bearer token
- **THEN** the agent never produces a plan and the existing 401 path is returned unchanged

### Requirement: Tool step arguments resolve from earlier step outputs at execution time

A `tool` step's `args` MAY reference the output of an earlier step in the same plan by that step's identifier, using an explicit reference syntax. A reference is a whole argument value, not embedded interpolation; the executor substitutes the referenced output value unchanged, and the tool's own schema decides whether that value is acceptable. The executor SHALL resolve the reference at execution time — after the referenced step has completed — and SHALL dispatch the tool with the resolved arguments; the planner never has to know a value it can only obtain by running a prior step (a composed body). A reference to a step that does not exist in the plan, has not completed, or that would form a cycle SHALL fail the step with a typed error and MUST NOT dispatch a partially-resolved argument. Arguments containing no reference MUST continue to dispatch exactly as authored, so plans without references are unaffected. A model-corrected call produced by the tool-retry path is dispatched literally and is not re-resolved as a template.

#### Scenario: Composed body reaches the write tool

- **WHEN** a plan contains a `tool` step whose argument references an earlier `llm` step's output, and that step has completed
- **THEN** the executor dispatches the tool with the earlier step's text substituted into the argument, and the tool receives the composed content rather than a plan-time placeholder

#### Scenario: Whole output value is substituted unchanged

- **WHEN** a tool step's argument is a reference to an earlier step whose output is a dict or list
- **THEN** the executor substitutes the whole output value, and the tool's schema decides whether it is acceptable; no path selector or partial extraction is performed

#### Scenario: Retry-corrected call is dispatched literally

- **WHEN** a tool call fails validation and the model produces a corrected call through the retry path
- **THEN** the corrected call's arguments are dispatched as authored, not re-resolved as templates

#### Scenario: Unknown reference fails the step closed

- **WHEN** a `tool` step's argument references a step identifier that is not in the plan
- **THEN** the step fails with a typed unresolved-reference error and no tool call is dispatched

#### Scenario: Forward or cyclic reference fails the step closed

- **WHEN** a `tool` step's argument references a step that has not yet run, or participates in a reference cycle
- **THEN** the step fails with a typed unresolved-reference error and no tool call is dispatched

#### Scenario: Referenced content is treated as untrusted at the dispatch boundary

- **WHEN** a resolved argument value derives from retrieved feed text, a fetched web page, or any other user-controlled output carrying untrusted-content framing markers
- **THEN** those markers are stripped from the dispatched argument before the tool call, so untrusted content cannot reach a tool as if it were planner-authored structure — the same handling retrieved context already receives before entering a prompt

#### Scenario: Plans without references are unaffected

- **WHEN** a `tool` step's arguments contain no reference syntax
- **THEN** the executor dispatches them exactly as authored, with no resolution pass and no behaviour change