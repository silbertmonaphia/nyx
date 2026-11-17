# Spec Delta

## MODIFIED Requirements

### Requirement: Partial-failure signal emitted when the run diverged from the plan

When an agent run terminates with any **system-recognised** divergence from the plan, the orchestrator SHALL emit a `note{kind: "partial_failure", failed_steps: [<step_id>, …]}` SSE frame **before** the final `delta` frames that carry the final answer whenever those deltas have not already been streamed. With per-step frame flushing, a divergence discovered after an earlier step's answer deltas were streamed SHALL be emitted no later than the end of the plan (or on a terminal error): the note then follows any already-streamed deltas and precedes any remaining delta frames. The frame is additive — old clients that ignore unknown note kinds continue to render the final answer unchanged.

#### Scenario: Tool retry exhaustion triggers the signal

- **WHEN** a `kind: "tool"` step exhausts its retry budget (per `agent-tool-retry`) and the final `tool_result` is `ok: false`
- **THEN** the orchestrator emits `note{kind: "partial_failure", failed_steps: [<step_id>]}` before the run's final `delta` frames when those deltas have not yet been streamed; the banner's `failed_steps` list contains the failing step's `id`

#### Scenario: Transient retry exhaustion triggers the signal

- **WHEN** a `kind: "tool"` step exhausts its transient-retry budget and the final `tool_result` is `ok: false`
- **THEN** the orchestrator emits `note{kind: "partial_failure", failed_steps: [<step_id>]}`; same semantics as the tool-arg path

#### Scenario: Intermediate retry failures do not trigger the signal

- **WHEN** a `tool` step's first attempt fails (`tool_result{ok: false, retry_count: 0}`) but a later attempt within the retry budget succeeds
- **THEN** the orchestrator does NOT emit `note{kind: "partial_failure"}`; the step's terminal `tool_result{ok: true, retry_count: N>0}` is the only signal of recovery

#### Scenario: Multiple failed steps aggregate into one signal

- **WHEN** two or more steps in the same run terminate with `step_failed`
- **THEN** the orchestrator emits a single `note{kind: "partial_failure", failed_steps: [<id1>, <id2>, …]}` before the final `delta` frames when those deltas have not yet been streamed; the frame's `failed_steps` list contains every failing step's `id` in plan order

#### Scenario: Plan-level errors trigger the signal

- **WHEN** the run terminates with `error{code: "plan_too_large"}`, `error{code: "plan_invalid"}`, `error{code: "react_iterations_exceeded"}`, `error{code: "budget_exceeded"}`, or `error{code: "budget_unavailable"}`
- **THEN** the orchestrator emits `note{kind: "partial_failure", failed_steps: <list>}` where `<list>` contains every step that ran and failed; the list is empty when the error fired before any step reached execution (e.g. `plan_too_large`), and the banner tells the user the run did not reach a terminal step

#### Scenario: Failure discovered after the answer's deltas were streamed

- **WHEN** an answering `llm`/`react` step's `delta` frames are streamed when that step completes, and a later step fails
- **THEN** the orchestrator emits `note{kind: "partial_failure", failed_steps: [<later step id>]}` after the already-streamed deltas and before the run's `done` / `terminator` frames; every frame is delivered exactly once, and the SPA still renders the banner above the final answer because it attaches the note to the assistant message by kind

#### Scenario: Non-fatal run-metadata errors do not trigger the signal

- **WHEN** the run terminates with `error{code: "run_metadata_unavailable"}` AND every step reached a terminal `ok` status
- **THEN** the orchestrator does NOT emit `note{kind: "partial_failure"}`; per `agent-run-state` the failure is non-fatal and the run completed as planned

#### Scenario: RAG empty result does not trigger the signal

- **WHEN** a `kind: "rag"` step returns `tool_result{ok: true, result: []}` (legitimate "no results")
- **THEN** the orchestrator does NOT emit `note{kind: "partial_failure"}`; an empty retrieval is a valid plan outcome, not a system-recognised failure
