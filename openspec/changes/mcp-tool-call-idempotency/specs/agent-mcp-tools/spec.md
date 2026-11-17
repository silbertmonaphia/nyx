# Spec Delta

## ADDED Requirements

### Requirement: Idempotency-key property is served but hidden from the model

When a registry tool's served `inputSchema` declares an optional `idempotency_key` property, the MCP `tools/list` surface SHALL expose it with a description telling clients to derive one deterministic key per logical operation, reuse it for identical retries, use a new key for a new operation, and omit it to opt out of deduplication. The agent's normalization SHALL exclude the property from the model-bound function schema and from the argument-schema text composed for retry re-prompts, so the model is never asked to produce or correct it. The dispatch funnel SHALL determine that a tool accepts a key from the raw served schema, not from the model-bound schema. The property MUST remain optional, and its presence MUST NOT change planner-catalog rendering, the confirmation policy, or retry classification.

#### Scenario: Served schema advertises the key with guidance

- **WHEN** MCP `tools/list` returns `create_feed`
- **THEN** its `inputSchema` includes an optional `idempotency_key` property whose description states the reuse-on-retry and new-key-per-operation semantics

#### Scenario: Model-bound schema excludes the property

- **WHEN** the agent normalizes `create_feed` into the function schema bound to the model
- **THEN** that schema does not contain `idempotency_key`, and the model's call arguments omit it

#### Scenario: Retry re-prompt schema excludes the property

- **WHEN** a `create_feed` call is rejected and the executor composes the argument-schema text for the re-prompt
- **THEN** that text does not include `idempotency_key`, while the next dispatch still injects the derived key

#### Scenario: Confirmation policy is unchanged

- **WHEN** a tool whose schema declares `idempotency_key` is dispatched through the confirmation gate
- **THEN** the same read-only / mutating classification applies as before the property was added — declaring the key neither requires nor suppresses confirmation
