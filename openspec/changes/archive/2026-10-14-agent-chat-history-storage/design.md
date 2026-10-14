# Design

## Context

The Python agent service streams SSE to the SPA but writes nothing — `agent.runs` records the run lifecycle, but the messages are gone the moment the stream ends. This design adds a persistent chat history store without changing the SSE wire contract (no new frames, no new fields on existing frames) and without adding any HTTP endpoints. See [`proposal.md`](./proposal.md) for motivation; see the `agent-chat-history-store` spec for the behavioral contract.

Existing patterns reused: `RunMetadata`'s asyncpg pool + retry posture, `RetentionCron`'s interval loop + OTel span + counter, `agent.runs`'s soft-delete + grace pattern (mirrored from the run-retention path, simplified for now — Slice 4 will own the read side).

## Goals / Non-Goals

**Goals:**
- Persist every chat turn (`user`, `assistant`, `tool`, `system`) with byte-faithful `tool_calls` JSON so future replay is lossless
- Single retention cron that runs both run + chat iterations; failure isolation between them
- One-year retention floor with a kill-switch env var
- Best-effort writes: a failed write never breaks the user-visible stream
- Cross-user isolation enforced at the SQL layer (`WHERE user_id=$X`) matching `RunMetadata`'s posture

**Non-Goals:**
- Any HTTP read endpoint (Slice 2)
- Any frontend change (Slice 2)
- GDPR export/erase endpoints (Slice 4)
- User preference extraction from messages (Slice 3)
- Cross-session recall / semantic search (deferred per `FUTURE_AGENT.md` §2.1)
- Schema in any namespace other than `agent.*` (already established)

## Decisions

### D1. Two tables, not one — sessions and messages separate

Splitting `chat_sessions` and `chat_messages` mirrors the `agent.runs` / langGraph split: sessions carry the user-facing list-row data (id, user_id, title, last_active_at, ended_at); messages carry the per-turn record. This separation lets Slice 2's sidebar endpoint query sessions cheaply (one row per session, indexed on `(user_id, last_active_at desc)`) and Slice 4's GDPR export stream all messages for one user without touching the session list. The alternative — one wide table with `session_id` nullable — would force every list query to scan all messages; rejected.

### D2. `id` is the run_id uuid4 — no new identifier namespace

A chat session IS a run, by definition: the existing `run_id` SSE frame already gives every chat a uuid4. Reusing that id as the `chat_sessions.id` PK means:
- The resume path needs no new identifier to look up — same `run_id` flows through `agent.runs`, `chat_sessions`, and langGraph `checkpoint_*`
- The `/agent/run` response carries `run_id` in its first frame, which is also the sidebar session key in Slice 2 — zero client-side mapping
- The cross-runtime wire contract stays unchanged

Alternative considered: a separate `chat_session_id` (sequence or uuid4) distinct from `run_id`. Rejected because it would force a mapping table on every read and create two different "session ids" in the system (operator confusion, plus a join on every replay query).

### D3. `tool_calls` as `jsonb`, not a separate table

Tool calls are part of the message envelope — replaying a message requires the same `tool_calls` JSON the SPA sees on the wire. A `jsonb` column preserves the byte-faithful shape; a separate `chat_tool_calls` table would force a join on every replay and risk field-name drift. Cost: harder to query "which sessions used tool X" — but no caller in Slice 1 (or Slices 2/3/4) needs that query. If a future slice needs it, a materialized view or `jsonb_path_ops` GIN index is the upgrade path; not now.

### D4. Soft-delete + grace period, mirroring the existing run-retention posture

`agent.runs` already chose soft-delete (`deleted_at`) + grace period; the chat store uses the same pattern so operators see one consistent retention story. The grace period is the buffer between "data is past retention" and "data is gone" — Slice 4 will own the read side of soft-deleted rows (GDPR export needs to surface them; GDPR erase needs to find them). For Slice 1, soft-deleted rows are not yet read by any consumer — but they're written so Slice 4 doesn't need a migration to add the column.

### D5. Single retention cron, two iterations per tick

The existing `RetentionCron` already runs `agent.runs` deletion on every tick. Adding `agent.chat_sessions` deletion to the same loop keeps the operational surface small (one task, one config knob for retention interval) and the failure isolation is trivial: wrap each iteration's body in a separate try/except inside the existing loop. Alternative considered: a second cron with its own interval. Rejected because the retention interval is the same setting for both surfaces, and a second cron doubles the heartbeat / metrics noise.

### D6. Writes are synchronous on the request hot path, not backgrounded

`record_message` is called inline at the SSE generator's `done` frame (before the stream closes). Backgrounding it would make Slice 2's "replay last session" query potentially miss the last message of a session — bad UX. Cost: each chat adds 2–3 round-trip writes (session row + per-message rows) on the hot path; acceptable because the pool is local Postgres with a 5s acquire timeout, and a write failure already degrades to "WARN + continue" (D7).

### D7. Write failures degrade to warn-log, never block the stream

`ChatStoreUnavailable` (raised on retry exhaustion) is caught by the SSE consumer in `main.py`, logged at WARN, and the run continues to `terminator`. This matches the existing `RunMetadataUnavailable` posture and keeps the user-visible stream functional even when Postgres is slow. The retry budget (3 attempts, 100ms→200ms→400ms) matches `RunMetadata._with_retry`; the asymmetry — chat history is allowed to silently drop rows when Postgres is sick — is acceptable because the streaming answer has already been delivered and Slice 4 (GDPR) has its own data-integrity story for backfill.

### D8. `last_active_at` is touched at every recorded message

A session's "last activity" for retention is when its last message was recorded, not when the run's `terminator` frame fired. This means a chat that goes silent mid-stream (network drop, user closes tab) is still retained from when it was last touched — correct UX, no extra code path beyond a single `UPDATE` per message write. The retention cron uses `last_active_at` as its predicate; alternative would be to use `ended_at` (when the run terminated), but `ended_at` is only set by `mark_completed` / `mark_failed` and would not reflect chats that crashed mid-stream.

### D9. No new pool — reuse `RunMetadata`'s asyncpg pool

The chat store takes the same `asyncpg.Pool` instance the run metadata layer already holds. Creating a second pool would double the connection budget for marginal benefit (both layers hit the same DB with the same credentials). The pool's `max_size` is governed by `PYTHON_AGENT_DATABASE_POOL_MAX_SIZE` (already exists; no change).

## Risks / Trade-offs

- **[Increased write load on the chat hot path]** → Each chat turn adds 1 INSERT for the message plus 1 UPDATE on `chat_sessions.last_active_at`. With `last_active_at` indexed for the sidebar query, the UPDATE is on a hot index path. Mitigation: index choice is `(user_id, last_active_at desc)` so the UPDATE only touches the index for that user's row, not the whole index. Monitoring: track `pg_stat_user_tables.n_tup_upd` on `chat_sessions` after the first prod week; if hot, switch the `last_active_at` update to a `LOWER(precision)` column or a debounced flush.
- **[Soft-delete rows are not yet read by any consumer]** → Operators will see `deleted_at` populated in queries but no UI surfaces them. Slice 4 will own the read side; until then, a `SELECT * FROM agent.chat_sessions WHERE deleted_at IS NOT NULL` is the manual check. Acceptable — Slice 4 is the next-but-one slice and the column is harmless.
- **`tool_calls` jsonb schema drift]** → The wire envelope's `tool_calls` shape is owned by `agent-langgraph` and may evolve (the `tool_dispatch.py` change already added retry metadata). Mitigation: every Slice 1 test that records a tool-bearing message round-trips the exact JSON via a frozen fixture; if the wire envelope changes, a test fails loudly and the migration is a column drop + re-add with a new comment, not a data-loss event (Slice 1 has no production data to migrate).
- **[Retention cron coupled to run-retention cadence]** → If the operator tunes `PYTHON_AGENT_RETENTION_INTERVAL_SECONDS` for chat-specific needs, they affect run retention too. Mitigation: the env var already exists with the same semantics for both surfaces; Slices 2/3/4 do not introduce a divergent knob. If a future slice needs per-surface cadence, the env var splits cleanly without breaking the existing setting.
- **[Cross-user isolation depends on `user_id` in the WHERE clause everywhere]** → Same risk `RunMetadata` already has. Mitigation: every chat store method takes `user_id` as a required argument and includes it in the WHERE clause; this matches the existing `RunMetadata` posture exactly. The Pattern is established; the new code inherits it.

## Migration Plan

The migration is the alembic `0004_chat_sessions_and_messages.py` revision; rollback is the corresponding `downgrade()`. No backfill is needed — both tables start empty. The schema (`agent.*`) already exists; the migration only adds two new tables and two indexes.

Deployment: same as every other Python agent deploy — `migrations_disabled=false` in compose runs alembic on boot via the lifespan; the dedicated `agent-migrate` one-shot service handles the prod path.

Rollback strategy:
- Code rollback: revert the commit; alembic `downgrade` drops both tables (no prod data is at risk in Slice 1 since this is the first time the tables exist).
- Operational rollback without code change: `PYTHON_AGENT_CHAT_RETENTION_ENABLED=false` halts deletion. Writes continue (so a future flip-back resumes normal retention); reads do not exist yet in Slice 1.
- Worst case (bad alembic on boot): the lifespan hook logs the alembic error and the service refuses to start. The previous alembic revision (`0003_add_run_heartbeat`) is still present in the version table; rolling back the alembic chain manually restores the pre-Slice-1 state.

## Open Questions

None. The cross-cutting decisions (retention floor, no new pool, soft-delete + grace, wire-stability) are all resolved in `AgentMemoryRoadmap.md`; this design implements them without further ambiguity.
