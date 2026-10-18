# Spec Delta

## MODIFIED Requirements

### Requirement: Persistent session and message store

The Python agent service SHALL persist every chat turn to two new tables in the `agent.*` schema:
- `agent.chat_sessions(id, user_id, title, started_at, last_active_at, ended_at, deleted_at)` — one row per chat session, with `id` being the same uuid4 emitted as the `run_id` SSE frame
- `agent.chat_messages(id, session_id, role, content, tool_calls jsonb, steps jsonb NULL, tokens int, prompt_tokens int NULL, completion_tokens int NULL, created_at)` — one row per message within a session; `role` is one of `user`, `assistant`, `tool`, `system`; `tool_calls` carries the JSON array of tool invocations emitted on the wire for assistant messages (NULL for non-tool-bearing roles); `steps` carries the assistant row's step trace (NULL for user / tool / system rows, for runs that planned no steps, and for rows persisted before the column existed); `tokens` is the provider-reported total token count for that message (0 when unknown); `prompt_tokens` / `completion_tokens` carry the split from the run's terminal `done` frame, NULL on rows persisted before the split columns existed (the replay then shows the total only)

The `steps` trace SHALL be a JSON array in plan order. Each entry SHALL carry `id`, `kind` (`rag` | `tool` | `llm` | `react`), and terminal `status` (`ok` | `failed`; `running` only when the run terminated without a `step_end`), plus the step's latest `tool_name`, `tool_args`, `tool_result`, integer `retry_count`, and failure `reason` when the wire supplied them. A step SHALL appear exactly once: `step_start`, `tool_call`, `tool_result`, and `step_end` frames for the same step id fold into one entry (latest value per field). A step that awaited approval MAY record no approval-card state — approval cards are not persisted.

The store SHALL be backed by an asyncpg connection pool (the existing pool from `run_metadata.py`) and SHALL retry transient `asyncpg.PostgresConnectionError` with exponential backoff matching the existing `RunMetadata` posture. A write failure that exhausts the retry budget SHALL NOT fail the user-visible chat — the streaming response reaches `done` and `terminator` regardless. The store SHALL raise `ChatStoreUnavailable` on exhausted retries; the SSE consumer logs at WARN and continues.

#### Scenario: A chat run leaves rows in both tables

- **WHEN** an authenticated user sends a chat that produces a 2-step plan (one tool call, one llm) and a final assistant answer
- **THEN** `agent.chat_sessions` contains exactly one row with `user_id=<user>`, `started_at <= ended_at`, `last_active_at = ended_at`
- **AND** `agent.chat_messages` contains one `user` row for the inbound message and one `assistant` row for the run, the assistant row carrying the run's whole step trace in `steps` and its tool invocations in `tool_calls`
- **AND** the streaming response itself is byte-identical to today's behavior (no new SSE frames, no new fields on existing frames)

#### Scenario: Native steps persist a trace without tool calls

- **WHEN** a completed run plans exactly `rag` then `llm` and dispatches no tools
- **THEN** the assistant row's `tool_calls` is NULL and its `steps` has exactly two entries in plan order, with `kind` `rag` and `llm` and terminal status `ok`
- **AND** a replay of the session can render both steps

#### Scenario: Tool frames fold into one step entry

- **WHEN** a tool step is dispatched, retried once, and ends `ok`
- **THEN** its single `steps` entry carries the latest `tool_name`, `tool_args`, `tool_result`, and `retry_count=1`; no duplicate entry is persisted

#### Scenario: Session boundary on resume echoes the existing session id

- **WHEN** a request echoes a `run_id` that already exists in `agent.chat_sessions` for the requesting `user_id`
- **THEN** the store reuses the existing session (no new `chat_sessions` row) and continues appending messages to it; `last_active_at` is refreshed at run end

#### Scenario: Cross-user session isolation is enforced

- **WHEN** a request echoes a `run_id` that exists in `agent.chat_sessions` but for a different `user_id`
- **THEN** the store SHALL treat it as a fresh run: a new session row is created with a new uuid4 and the request streams under the new id (matches the existing resume-decision posture in `main.py:477-482`)

#### Scenario: Async write failure does not break the stream

- **WHEN** `record_message` exhausts its retry budget
- **THEN** the SSE consumer logs the failure at WARN and the run continues to `done` and `terminator`; the missing row is not retried later (best-effort only)
