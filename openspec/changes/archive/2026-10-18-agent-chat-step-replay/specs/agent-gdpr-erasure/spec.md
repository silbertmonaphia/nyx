# Spec Delta

## ADDED Requirements

### Requirement: Exported messages include the agent step trace

The GDPR export (`GET /agent/me/export`) SHALL include a `steps` field on every exported message row, carrying the same persisted step-trace array the replay endpoint returns, or `null` when no trace exists (user rows, pre-migration rows, and runs that planned no steps). The field is additive; every existing export field and the owner scoping of the export are unchanged.

#### Scenario: Export carries the trace for agent turns

- **WHEN** a user with a completed `[rag, llm]` turn calls `GET /agent/me/export`
- **THEN** the exported assistant message row carries the two-entry `steps` array, and the exported user row carries `steps: null`

#### Scenario: Legacy rows export null steps

- **WHEN** an exported assistant row was persisted before the `steps` column existed
- **THEN** its `steps` field is `null`; the rest of the row is exported unchanged
