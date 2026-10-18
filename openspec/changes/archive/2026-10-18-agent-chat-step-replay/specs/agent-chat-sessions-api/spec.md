# Spec Delta

## MODIFIED Requirements

### Requirement: Per-session message replay

The service SHALL expose `GET /agent/sessions/{session_id}/messages` returning `{messages: [{id, role, content, tool_calls, steps, tokens, prompt_tokens, completion_tokens, created_at}]}` ordered by `created_at ASC, id ASC`. `steps` is the persisted step-trace array (same entry shape as `agent-chat-history-store`) or `null`; it is `null` on user rows and on rows persisted before the `steps` column existed. `prompt_tokens` / `completion_tokens` are `null` on rows persisted before the usage-split columns existed; a reopened chat panel then renders the total only. The session MUST belong to the requesting user (`X-Nyx-User-Id`); otherwise 404. Soft-deleted sessions return 404 (consistent with the list filter).

#### Scenario: Replay a multi-message session

- **WHEN** a user requests messages for a session that recorded 1 user message and 1 assistant message
- **THEN** the response carries both messages in chronological order, with the assistant's `tool_calls` (if any) verbatim from the original SSE envelope
- **AND** the assistant's `steps` is the persisted trace; the user row's `steps` is `null`

#### Scenario: Replay preserves native rag and llm steps

- **WHEN** the recorded assistant row was produced by a `[rag, llm]` plan with no tool dispatches
- **THEN** the replay response carries two `steps` entries with `kind` `rag` and `llm` and their terminal statuses, and `tool_calls` is `null`

#### Scenario: Legacy rows replay with null steps

- **WHEN** a message row was persisted before the `steps` column existed
- **THEN** the response carries `steps: null` for that row; the client MAY fall back to deriving steps from `tool_calls`

#### Scenario: Cross-user replay 404

- **WHEN** user A requests messages for user B's session
- **THEN** the service returns HTTP 404 (not 403, to avoid leaking the existence of user B's session ids)

#### Scenario: Soft-deleted session replay 404

- **WHEN** a session is soft-deleted (`deleted_at IS NOT NULL`)
- **THEN** `GET /agent/sessions/{id}/messages` returns 404; the messages still exist in the table but are not exposed
