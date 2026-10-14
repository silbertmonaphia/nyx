# agent-chat-history-store Specification

## Purpose
Persists every chat turn the Python agent processes to durable storage so the operator can analyze user behavior, the user can replay their history, and downstream slices (preference memory, GDPR export/erase) have a stable data plane to build on. Slice 1 ships only the storage layer — no read endpoints, no UI.

## Requirements

### Requirement: Persistent session and message store

The Python agent service SHALL persist every chat turn to two new tables in the `agent.*` schema:
- `agent.chat_sessions(id, user_id, title, started_at, last_active_at, ended_at, deleted_at)` — one row per chat session, with `id` being the same uuid4 emitted as the `run_id` SSE frame
- `agent.chat_messages(id, session_id, role, content, tool_calls jsonb, tokens int, created_at)` — one row per message within a session; `role` is one of `user`, `assistant`, `tool`, `system`; `tool_calls` carries the JSON array of tool invocations emitted on the wire for assistant messages (NULL for non-tool-bearing roles); `tokens` is the provider-reported token count for that message (0 when unknown)

The store SHALL be backed by an asyncpg connection pool (the existing pool from `run_metadata.py`) and SHALL retry transient `asyncpg.PostgresConnectionError` with exponential backoff matching the existing `RunMetadata` posture. A write failure that exhausts the retry budget SHALL NOT fail the user-visible chat — the streaming response reaches `done` and `terminator` regardless. The store SHALL raise `ChatStoreUnavailable` on exhausted retries; the SSE consumer logs at WARN and continues.

#### Scenario: A chat run leaves rows in both tables

- **WHEN** an authenticated user sends a chat that produces a 2-step plan (one tool call, one llm) and a final assistant answer
- **THEN** `agent.chat_sessions` contains exactly one row with `user_id=<user>`, `started_at <= ended_at`, `last_active_at = ended_at`
- **AND** `agent.chat_messages` contains one `user` row for the inbound message and one `assistant` row per step + the final answer, each with `tool_calls` populated only when the assistant emitted tool calls on the wire
- **AND** the streaming response itself is byte-identical to today's behavior (no new SSE frames, no new fields on existing frames)

#### Scenario: Session boundary on resume echoes the existing session id

- **WHEN** a request echoes a `run_id` that already exists in `agent.chat_sessions` for the requesting `user_id`
- **THEN** the store reuses the existing session (no new `chat_sessions` row) and continues appending messages to it; `last_active_at` is refreshed at run end

#### Scenario: Cross-user session isolation is enforced

- **WHEN** a request echoes a `run_id` that exists in `agent.chat_sessions` but for a different `user_id`
- **THEN** the store SHALL treat it as a fresh run: a new session row is created with a new uuid4 and the request streams under the new id (matches the existing resume-decision posture in `main.py:477-482`)

#### Scenario: Async write failure does not break the stream

- **WHEN** `record_message` exhausts its retry budget
- **THEN** the SSE consumer logs the failure at WARN and the run continues to `done` and `terminator`; the missing row is not retried later (best-effort only)

### Requirement: Retention floor of at least one year

The retention cron SHALL hard-delete `chat_sessions` rows whose `last_active_at < now() - PYTHON_AGENT_CHAT_RETENTION_DAYS * interval '1 day'`. The default and minimum value of `PYTHON_AGENT_CHAT_RETENTION_DAYS` SHALL be 365 (one year). For each deleted session, the cron SHALL also delete its `chat_messages` rows. `PYTHON_AGENT_CHAT_RETENTION_ENABLED=false` SHALL disable the cron entirely (rollback path).

#### Scenario: Retention honors the floor

- **WHEN** `PYTHON_AGENT_CHAT_RETENTION_DAYS=365` and a session's `last_active_at` is 366 days in the past
- **THEN** the next retention iteration deletes the session row and all its message rows
- **AND** no error is raised if no rows match the predicate (an empty run is normal)

#### Scenario: Below-floor retention refuses startup

- **WHEN** `PYTHON_AGENT_CHAT_RETENTION_DAYS` is set to a value < 365
- **THEN** the Settings validator SHALL raise `ValueError` and the service SHALL refuse to start

#### Scenario: Kill switch disables retention without code change

- **WHEN** `PYTHON_AGENT_CHAT_RETENTION_ENABLED=false`
- **THEN** the retention cron SHALL NOT start (the lifespan hook skips its creation), no rows are deleted, and no `agent_chat_retention_deleted_total` increments occur

### Requirement: Soft-delete with grace period before hard-delete

The retention cron SHALL use a two-phase delete: rows whose `last_active_at` is past the retention window are soft-deleted (`deleted_at = now()`) on the first iteration that observes them, and hard-deleted on the iteration where `deleted_at < now() - PYTHON_AGENT_CHAT_RETENTION_GRACE_DAYS * interval '1 day'`. The default `PYTHON_AGENT_CHAT_RETENTION_GRACE_DAYS` SHALL be 7. Slice 4 (GDPR export/erase) is the only consumer that reads the soft-deleted state; until Slice 4 lands, soft-deleted rows SHALL be invisible to all read paths in Slice 1.

#### Scenario: Soft-delete then hard-delete

- **WHEN** a session's `last_active_at` is past the retention window
- **THEN** the first retention iteration sets `deleted_at = now()` but does NOT delete the row
- **AND** a subsequent iteration after `PYTHON_AGENT_CHAT_RETENTION_GRACE_DAYS` has elapsed removes the row (and its `chat_messages` rows) via `DELETE`
- **AND** `agent_chat_retention_deleted_total` increments by the number of sessions hard-deleted (soft-deletes are not counted)

#### Scenario: Grace period can be zero

- **WHEN** `PYTHON_AGENT_CHAT_RETENTION_GRACE_DAYS=0`
- **THEN** the retention iteration behaves as a single-phase hard-delete on the same iteration that observes the expired row

### Requirement: Retention runs as a single cron

The retention cron SHALL run on the same cadence and in the same asyncio task as the existing `agent.runs` retention cron. The chat retention iteration SHALL execute after the run retention iteration in the same loop. A failure in the chat retention iteration SHALL NOT abort the run retention iteration, and vice versa. Both iterations share the `agent.run.retention` / `agent.chat.retention` OTel span boundary; the chat iteration increments `agent_chat_retention_deleted_total` and reports its deleted count via span attribute `chat_deleted_count`.

#### Scenario: Cron wiring

- **WHEN** the lifespan starts
- **THEN** the existing `RetentionCron` is constructed with a `ChatStore` reference alongside its `RunMetadata` reference
- **AND** the cron task emits `agent.run.retention` then `agent.chat.retention` on each tick

#### Scenario: One retention failure does not block the other

- **WHEN** the run-retention iteration raises an exception
- **THEN** the chat-retention iteration still runs on the same tick (caught at the loop level)
- **AND** the next tick runs both iterations again
