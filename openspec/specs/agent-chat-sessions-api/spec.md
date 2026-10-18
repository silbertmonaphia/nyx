# agent-chat-sessions-api Specification

## Purpose
Lets users list, replay, create, resume, and delete their chat sessions via HTTP — the read-side surface over the Slice 1 storage layer. Every endpoint is owner-scoped via `X-Nyx-User-Id`; cross-user requests 404. No new SSE frames; the existing `/agent/run` SSE flow is untouched.

## Requirements

### Requirement: Paginated session list

The service SHALL expose `GET /agent/sessions?limit=<n>&before=<cursor>` returning a JSON object with `sessions: [{id, user_id, title, started_at, last_active_at, ended_at, message_count}]` and `next_cursor: <cursor-or-null>`. `limit` MUST be in `[1, 50]` (default 10); invalid values 400. Sessions SHALL be ordered by `(last_active_at DESC, id DESC)`; soft-deleted sessions SHALL NOT appear. The cursor encodes the last row's `(last_active_at, id)` tuple so pagination is stable across same-second updates.

#### Scenario: Three sessions paginate

- **WHEN** an authenticated user has 3 sessions with last_active_at T1 < T2 < T3
- **THEN** `GET /agent/sessions` returns T3 first, T2 second, T1 third, with `next_cursor` non-null until the last page
- **AND** `GET /agent/sessions?limit=2` returns T3, T2, and `next_cursor=<T2-cursor>`; following that cursor returns T1 with `next_cursor=null`

#### Scenario: Soft-deleted sessions never appear

- **WHEN** a session has `deleted_at IS NOT NULL`
- **THEN** it is invisible to the list endpoint (the retention cron owns the soft-delete state; Slice 4 owns the read-side)

#### Scenario: Cross-user list is owner-scoped

- **WHEN** user A calls `GET /agent/sessions`
- **THEN** only user A's sessions appear; user B's sessions are not visible (no shared / global list)

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

### Requirement: Session creation and resume

The service SHALL expose `POST /agent/sessions` creating an empty session with `title=""`, returning `{session_id: <uuid4>, started_at: <iso8601>}`. The session is owned by the requesting user and starts with no messages.

The service SHALL expose `POST /agent/sessions/{session_id}/resume` returning `{session_id: <uuid4>, resumed_at: <iso8601>}` — the operation touches `last_active_at` so the retention cron sees the session as live and the sidebar ranks it at the top. Cross-user or soft-deleted session returns 404.

#### Scenario: New session is empty

- **WHEN** a user posts `POST /agent/sessions`
- **THEN** the response carries a fresh uuid4 + the current timestamp; the session has 0 messages until the next `/agent/run` populates it

#### Scenario: Resume refreshes the sidebar order

- **WHEN** a user resumes a 7-day-old session
- **THEN** `last_active_at` is updated to `now()`; the next `GET /agent/sessions` returns this session at the top of the list

### Requirement: User-initiated session deletion

The service SHALL expose `DELETE /agent/sessions/{session_id}` setting `deleted_at = now()` on the session (soft-delete — Slice 4's GDPR export/erase is the only consumer that ever reads soft-deleted state, and the retention cron will eventually hard-delete the row). Cross-user or non-existent session returns 404. Messages cascade via `ON DELETE CASCADE` only when the retention cron hard-deletes; the soft-delete here leaves the messages in the table until then.

#### Scenario: Delete hides the session immediately

- **WHEN** a user deletes session S
- **THEN** `GET /agent/sessions` no longer lists S, and `GET /agent/sessions/S/messages` returns 404 — even before the retention cron runs

#### Scenario: Cross-user delete 404

- **WHEN** user A deletes user B's session
- **THEN** the service returns HTTP 404 (the WHERE clause filters by user_id; the row is invisible)

### Requirement: Health and observability

The new endpoints SHALL NOT affect the existing `GET /healthz` posture (it remains a pure Redis reachability check). Each endpoint MUST emit a per-request OTel span on the existing `nyx.agent` tracer with attributes `endpoint`, `user_id`, and (for list / replay) `result_count` so the rollout sibling can verify traffic volume post-deploy. No new Prometheus counters — the existing `agent_runs_total` / `agent_steps_total` already cover agent activity; session list / replay are passive reads.

#### Scenario: Per-request span attributes are populated

- **WHEN** `GET /agent/sessions` returns 3 sessions
- **THEN** the trace shows the span with `endpoint="list_sessions"`, `user_id=<the user>`, `result_count=3`
