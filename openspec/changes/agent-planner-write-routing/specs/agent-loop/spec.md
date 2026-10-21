# Spec Delta

## ADDED Requirements

### Requirement: Tool step arguments resolve from earlier step outputs at execution time

A `tool` step's `args` MAY reference the output of an earlier step in the same plan by that step's identifier, using an explicit reference syntax. The executor SHALL resolve the reference at execution time — after the referenced step has completed — and SHALL dispatch the tool with the resolved arguments; the planner never has to know a value it can only obtain by running a prior step (a concrete feed identifier, a composed body). A reference to a step that does not exist in the plan, has not completed, or that would form a cycle SHALL fail the step with a typed error and MUST NOT dispatch a partially-resolved argument. Arguments containing no reference MUST continue to dispatch exactly as authored, so plans without references are unaffected.

#### Scenario: Composed body reaches the write tool

- **WHEN** a plan contains a `tool` step whose argument references an earlier `llm` step's output, and that step has completed
- **THEN** the executor dispatches the tool with the earlier step's text substituted into the argument, and the tool receives the composed content rather than a plan-time placeholder

#### Scenario: Real identifier resolves instead of a placeholder

- **WHEN** a plan's `update_feed` step takes its `id` argument from an earlier step that listed the user's feeds, and that step returned concrete identifiers
- **THEN** the dispatched call carries a real identifier from that step's output rather than a plan-time placeholder, and the write can succeed

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