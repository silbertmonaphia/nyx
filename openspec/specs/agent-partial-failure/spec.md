# Agent Partial Failure Specification

## Purpose

Tells the user when an agent run finished with a step that did not match the plan, so the final answer is not silently presented as complete. The trigger is system-recognised failure only — RAG returning `[]` (legitimate "no results") and model hallucination are not in scope; those are separate product problems.

Scope note (recorded 2026-10-14): for plan-level errors the orchestrator emits the note, but the SPA's typed-error path drops the contentless assistant row before a banner can attach — for those runs the error toast is the user signal, and the banner renders only for runs where an answer exists (recovered / partial). This is the accepted reading of the UI requirement below, whose scope is "the assistant message's final-answer region".

## Requirements

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

### Requirement: Banner trigger is deterministic and testable

The set of SSE frames that constitute a "system-recognised failure" SHALL be exactly the following, in this priority order:
1. Any `error{code: "plan_too_large"}` / `plan_invalid` / `react_iterations_exceeded` / `budget_exceeded` / `budget_unavailable` frame
2. Any `tool_result{ok: false}` for a (step, tool) whose dispatch ended in a terminal failure after retries (recorded when the dispatch gives up — covers single-dispatch exhaustion, allowances split across ReAct iterations, and non-retryable rejections; a dispatch that recovered on a later attempt is excluded even when it consumed the full allowance)
3. Any `step_end` whose step's terminal status is `failed`

The orchestrator SHALL NOT classify a run as partial-failure on any other signal (RAG empty, model refusal, output-shape mismatch, latency). The `failed_steps` list SHALL contain every `step_id` whose `step_end` carried `status="failed"`, plus any step whose final `tool_result` was `ok: false` after retries.

#### Scenario: Implementation has a single source of truth for the trigger

- **WHEN** a contributor reads the executor code
- **THEN** there is exactly one function (e.g. `_collect_failed_steps(state) -> list[str]`) that the SSE publisher calls to decide whether to emit `note{kind: "partial_failure"}`; the trigger logic is not duplicated across step handlers

#### Scenario: Trigger function is unit-tested independently of the SSE publisher

- **WHEN** a test constructs an `AgentState` with a known set of failures (RAG empty, tool retry exhausted, plan_too_large, run_metadata_unavailable)
- **THEN** the test asserts `_collect_failed_steps(state)` returns the expected `step_id` list — RAG empty and run_metadata_unavailable absent, the others present

### Requirement: UI renders a partial-failure banner above the final answer

When the chat client receives a `note{kind: "partial_failure", failed_steps: [...]}` frame, the assistant message's final-answer region SHALL render a `<PartialFailureBanner>` component above the `delta` text, showing the list of failed step ids and a human-readable message ("Some steps failed — the answer below may be incomplete"). The banner SHALL have a stable test id (`data-testid="chat-partial-failure-banner"`) and SHALL carry a `data-failed-steps` attribute containing the comma-joined step ids. The banner is rendered in addition to (not instead of) the per-step row in the existing `agent-steps` disclosure.

#### Scenario: Banner is visible without expanding the steps panel

- **WHEN** the user receives a chat response whose run had a failed step
- **THEN** the `<PartialFailureBanner>` is rendered directly above the final-answer text, visible without the user having to expand the `agent-steps` disclosure

#### Scenario: Banner is not rendered for clean runs

- **WHEN** the run terminates with no `note{kind: "partial_failure"}` frame
- **THEN** the assistant message renders no banner; `data-testid="chat-partial-failure-banner"` is not present in the DOM

#### Scenario: Banner lists the failed step ids

- **WHEN** the run had two failed steps (`step_2`, `step_4`)
- **THEN** the banner's `data-failed-steps` attribute equals `step_2,step_4` and the rendered text names both

### Requirement: Failed steps are visible to later model-facing steps

When a step terminates with `step_end{status: "failed"}`, every subsequent model-facing step (`llm` or `react`) in the same run SHALL receive a bounded context section describing the failed steps that precede it, in plan order. For a `tool` step the section SHALL name the tool and carry the safe error text already published on that step's terminal `tool_result{ok: false}`. For any other step kind the section SHALL carry a static failure description and MUST NOT carry exception class names, provider error text, or `step_end.reason` internals. The section SHALL be built only from text that is already safe to ship on the wire; it MUST NOT include raw exception messages or stack traces.

The section SHALL respect the existing prior-data bounds: per-section cap, total cap, and most-recent-first retention when the total cap drops sections. Alongside the section, the orchestrator SHALL add a static instruction directing the model to report the failure plainly and not present the failed action as successful. When no earlier step failed, the model-facing prompt SHALL remain byte-identical to its pre-change form.

#### Scenario: Approval expiry reaches the answering step

- **WHEN** a `create_feed` `tool` step fails because its confirmation expired, its terminal `tool_result{ok: false}` carries `Confirmation required: the approval request expired before a decision. The `create_feed` tool was not executed.`, and the plan continues to an answering `llm` step
- **THEN** the answering step's model input contains that safe error text and the static no-false-success instruction, so the answer can state the write did not happen

#### Scenario: Recovered retry contributes no failure context

- **WHEN** a `tool` step's first attempt fails but a later attempt within the retry budget succeeds and the step ends `ok`
- **THEN** no failed-step section is rendered and the answering step's prompt is byte-identical to a run with no failures

#### Scenario: Multiple failed steps are listed in plan order

- **WHEN** two failed steps precede the answering step
- **THEN** the section lists both step identifiers in plan order, each with its own safe error text, subject to the existing per-section and total caps

#### Scenario: Non-tool failure carries no internal detail

- **WHEN** an `llm` step fails with a provider exception and a later `llm` step answers
- **THEN** the failure section describes the step as failed without naming the exception class, the provider error text, or the raw `step_end` reason

#### Scenario: React step after a failure receives the context

- **WHEN** a step fails and the plan's next model-facing step is a `react` step
- **THEN** the react loop's model input contains the same failed-step section and instruction as an `llm` answering step

#### Scenario: Clean run prompt is unchanged

- **WHEN** every step before the model-facing step ended `ok`
- **THEN** no failed-step section or instruction is added, and the assembled prompt matches the pre-change prompt byte for byte
