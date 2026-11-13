# Spec Delta

## MODIFIED Requirements

### Requirement: Per-session message replay

The service SHALL expose `GET /agent/sessions/{session_id}/messages` returning `{messages: [{id, role, content, tool_calls, steps, tokens, prompt_tokens, completion_tokens, created_at}]}` ordered by `created_at ASC, id ASC`. `steps` is the persisted step-trace array (same entry shape as `agent-chat-history-store`) or `null`; it is `null` on user rows and on rows persisted before the `steps` column existed. `prompt_tokens` / `completion_tokens` are `null` on rows persisted before the usage-split columns existed; a reopened chat panel then renders the total only. The session MUST belong to the requesting user (`X-Nyx-User-Id`); otherwise 404. Soft-deleted sessions return 404 (consistent with the list filter).

The endpoint SHALL support optional pagination via `limit` and `before` query parameters. Without them, the response is the full chronological replay exactly as before (backward compatible — existing clients see no change). With `limit` present, the response returns the most recent `limit` messages (the newest window) still ordered chronologically (oldest first within the page), plus `has_more: <bool>` (true when at least one older message exists beyond the returned window) and `next_before: <string-or-null>` (an opaque cursor encoding the oldest returned message's id, null when `has_more` is false). `before` (present only together with `limit`) positions the window strictly below the boundary message the cursor encodes, so following `next_before` returns the page immediately older than the previous one. `limit` MUST be an integer in `[1, 200]`; `before` MUST be the verbatim `next_before` string a previous response on this endpoint minted (clients MUST NOT construct cursor values themselves); invalid values return 400. A page MAY be empty: a `before` older than the session's first message returns `{messages: [], has_more: false, next_before: null}`. Cross-user and soft-deleted sessions return 404 identically with and without pagination parameters.

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

#### Scenario: No parameters return the full replay unchanged

- **WHEN** a client calls the endpoint without `limit` or `before`
- **THEN** the response carries every message of the session in chronological order, with no `has_more` / `next_before` fields added — byte-compatible with the pre-pagination contract

#### Scenario: A limited request returns the newest window

- **WHEN** a session holds 7 messages and the client calls the endpoint with `limit=3`
- **THEN** the response carries messages 5, 6, 7 in chronological order (oldest first within the page)
- **AND** `has_more` is `true` and `next_before` encodes message 5's id

#### Scenario: Following the cursor assembles the older page

- **WHEN** the client requests `limit=3&before=<next_before from the previous page>`
- **THEN** the response carries messages 2, 3, 4 in chronological order with `has_more=true` and a cursor encoding message 2's id (the page's oldest INCLUDED row)
- **AND** a further page returns message 1 alone with `has_more=false` and `next_before=null` (the window is strictly below the cursor — the boundary row is never repeated)

#### Scenario: A cursor below the first message returns an empty page

- **WHEN** the client follows a cursor that positions the window strictly below the session's first message
- **THEN** the response is `{messages: [], has_more: false, next_before: null}` (HTTP 200, not 404)

#### Scenario: Invalid pagination parameters are rejected

- **WHEN** the client calls the endpoint with `limit=0`, `limit=201`, `limit=abc`, or a `before` that is not a cursor this endpoint minted
- **THEN** the service returns HTTP 400

#### Scenario: Pagination keeps owner scoping and soft-delete visibility

- **WHEN** user A calls the endpoint with `limit=10` on user B's session, or on a session user A soft-deleted
- **THEN** the response is HTTP 404 — identical to the unpaginated case
