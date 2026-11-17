# Spec Delta

## ADDED Requirements

### Requirement: Idempotency-key property survives schema normalization

When a registry tool's served `inputSchema` declares an optional `idempotency_key` property, normalization SHALL preserve that property in the bindable function schema and in the argument-schema text composed for retry re-prompts, so the shared dispatch funnel can identify which tools accept a key. The property MUST remain optional. Its presence MUST NOT change planner-catalog rendering, the confirmation policy, or retry classification.

#### Scenario: Bound schema preserves the property

- **WHEN** MCP `tools/list` serves `create_feed` with an optional `idempotency_key` property
- **THEN** the function the model is bound to exposes `idempotency_key` as an optional parameter, alongside the existing required fields

#### Scenario: Retry re-prompt schema carries the property

- **WHEN** a `create_feed` call is rejected and the executor composes the argument-schema text for the re-prompt
- **THEN** that text includes the optional `idempotency_key` property rather than a schema missing it

#### Scenario: Confirmation policy is unchanged

- **WHEN** a tool whose schema declares `idempotency_key` is dispatched through the confirmation gate
- **THEN** the same read-only / mutating classification applies as before the property was added — declaring the key neither requires nor suppresses confirmation
