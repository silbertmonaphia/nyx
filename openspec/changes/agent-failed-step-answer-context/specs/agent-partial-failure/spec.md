# Spec Delta

## ADDED Requirements

### Requirement: Failed steps are visible to later model-facing steps

When a step terminates with `step_end{status: "failed"}`, every subsequent model-facing step (`llm` or `react`) in the same run SHALL receive a bounded context section describing the failed steps that precede it, in plan order. For a `tool` step the section SHALL name the tool and carry the safe error text already published on that step's terminal `tool_result{ok: false}`. For any other step kind the section SHALL carry a static failure description and MUST NOT carry exception class names, provider error text, or `step_end.reason` internals. The section SHALL be built only from text that is already safe to ship on the wire; it MUST NOT include raw exception messages or stack traces.

The section SHALL respect the existing prior-data bounds: per-section cap, total cap, and most-recent-first retention when the total cap drops sections. Alongside the section, the orchestrator SHALL add a static instruction directing the model to report the failure plainly and not present the failed action as successful. When no earlier step failed, the model-facing prompt SHALL remain byte-identical to its pre-change form.

#### Scenario: Approval expiry reaches the answering step

- **WHEN** a `create_feed` `tool` step fails because its confirmation expired, its terminal `tool_result{ok: false}` carries `Confirmation required: the approval request expired before a decision. The create_feed tool was not executed.`, and the plan continues to an answering `llm` step
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
