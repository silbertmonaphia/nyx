# Spec Delta

## ADDED Requirements

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
