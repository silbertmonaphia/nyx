# Spec Delta

## ADDED Requirements

### Requirement: Retry instruction after a reported write failure re-plans the write

When the most recent user message is a retry instruction from a fixed lexical cue set, and the immediately preceding assistant message reports that a specific feed write tool (`create_feed`, `update_feed`, or `delete_feed`) was not executed, the emitted plan SHALL contain a `tool` step invoking that named tool before the answering step. The retried write SHALL follow the same rules as an explicit state-change request: compose-before-write when the body must be produced, the existing content limit, and a new confirmation request before dispatch — a previously expired or denied approval MUST NOT be reused.

Both signals are required. The guard MUST NOT fire when the latest message carries no retry cue, when the preceding assistant text names no write tool as not executed, when a negation or deferral qualifier is present (for example 别重试, 不要重试, 先不要重试), or when the preceding assistant text reports success. The cue set ships Chinese phrasing only, matching the existing state-change cue scope; English retry phrasing relies on the planner prompt rather than the deterministic guard.

#### Scenario: Retry after an expired confirmation re-plans the write

- **WHEN** the preceding assistant message reports that `create_feed` was not executed because the approval request expired, and the latest user message is 再试试
- **THEN** the plan contains a `create_feed` `tool` step before the answering step, with a composing `llm` step when the body must be produced, and dispatch raises a fresh confirmation request

#### Scenario: Retry cue without a named failed write is not guard-triggered

- **WHEN** the latest user message is 继续 but the preceding assistant message names no write tool as not executed
- **THEN** the retry guard does not fire and the plan follows the existing rules for the message's actual intent

#### Scenario: Negated retry is not guard-triggered

- **WHEN** the preceding assistant message reports a failed `create_feed` and the latest user message is 先不要重试
- **THEN** the guard does not fire and no write tool step is mandated by it

#### Scenario: Explicit state-change phrasing keeps the existing mandate

- **WHEN** the preceding assistant message reports a failed write and the latest user message explicitly asks to 创建 or 发布
- **THEN** the existing state-change write-tool mandate applies unchanged, with no additional retry rule needed

#### Scenario: Reported success is not retried

- **WHEN** the preceding assistant message reports that the write succeeded and the latest user message is 再试试
- **THEN** the retry guard does not fire and no write tool step is mandated by it
