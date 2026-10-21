# Spec Delta

## ADDED Requirements

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