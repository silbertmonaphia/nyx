# Proposal

## Why

The Python agent service streams `chat`-shaped SSE frames to the SPA today but writes **nothing** to disk — `agent.runs` tracks the run lifecycle (created/completed/failed + tokens + error_code), but the actual messages and per-session boundaries are lost the moment the stream ends. Three downstream consequences, all in `AgentMemoryDesign.md`: (1) the operator has no record of what users are asking the agent — analytics on question categories, frequency, etc. is impossible without a parallel log scrape; (2) users lose access to their history the moment they close the tab — there is no sidebar, no replay, no "yesterday's conversation"; (3) future work on user-preference memory (`agent-user-preferences` slice) needs a place to anchor extracted facts (which session did this preference come from?) and a per-session thread to extract from. Slice 1 ships only the **storage layer** (the unmovable foundation) so Slices 2/3/4 each own one product capability on top.

## What Changes

- Add `agent.chat_sessions(id, user_id, title, started_at, last_active_at, ended_at, deleted_at)` and `agent.chat_messages(id, session_id, role, content, tool_calls jsonb, tokens int, created_at)` via alembic `0004`. Indexes: `(user_id, last_active_at desc)` for the sidebar query, `(session_id, created_at)` for message replay.
- New `ChatStore` (in `backend/agent_py/agent/chat_store.py`) — thin async wrapper over the two tables. Owns `create_session`, `record_message`, `touch_session`, `end_session`, and the retention queries (`soft_delete_expired`, `hard_delete_soft_deleted`). Same retry/backoff posture as `RunMetadata`.
- Extend the retention cron in `backend/agent_py/agent/retention.py` to **also** delete expired chat rows (single OTel span `agent.chat.retention`, new counter `agent_chat_retention_deleted_total`). Soft-delete + 7-day grace period, then hard-delete — mirrors the `agent.runs` retention posture and is the pattern Slice 4 (GDPR) will reuse for full account erasure.
- Wire writes into the existing SSE generator in `backend/agent_py/agent/main.py`: at run start `create_session` (or resume an existing one if `run_id` is echoed and found in `chat_sessions`); one `record_message` row per user message at request entry + per assistant message at the `done` frame (synchronous, before the Slice 3 async extraction). `tool_calls` are recorded verbatim from the SSE envelope so the replay is byte-faithful.
- New env vars on `Settings`: `PYTHON_AGENT_CHAT_RETENTION_DAYS=365` (≥1 year requirement), `PYTHON_AGENT_CHAT_RETENTION_GRACE_DAYS=7`, `PYTHON_AGENT_CHAT_RETENTION_ENABLED=true` (rollback kill-switch — flips retention off without code change).
- **No new HTTP endpoints**, **no frontend changes** — Slice 1 is purely the data plane. The sidebar endpoints live in Slice 2; the GDPR export/delete endpoints live in Slice 4.
- **Wire**: unchanged. Existing SSE frames (`run_id`, `plan`, `step_start`, `delta`, `done`, `terminator`, etc.) keep their byte shape. Slice 1 only adds **writes** to disk, not anything on the wire.

## Capabilities

### New Capabilities

- `agent-chat-history-store`: owns the `agent.chat_sessions` + `agent.chat_messages` tables, the `ChatStore` async wrapper, the retention cron extension, and the SSE-generator wiring that records each turn. Defines the retention floor (≥1 year) and the soft-delete + 7-day grace semantics.

### Modified Capabilities

- `agent-python-service`: env-var list gains the three retention settings (`PYTHON_AGENT_CHAT_RETENTION_DAYS`, `PYTHON_AGENT_CHAT_RETENTION_GRACE_DAYS`, `PYTHON_AGENT_CHAT_RETENTION_ENABLED`) and the `CHAT_RETENTION_DAYS >= 1` validator (365 floor is the user-facing requirement, not just a default). No HTTP / wire changes.
- `agent-observability`: counter list gains `agent_chat_retention_deleted_total`; OTel span list gains `agent.chat.retention`.

## Impact

- **Code**: `backend/agent_py/agent/chat_store.py` (new), `backend/agent_py/migrations/versions/0004_chat_sessions_and_messages.py` (new), `backend/agent_py/agent/retention.py` (extend existing cron), `backend/agent_py/agent/config.py` (3 new settings + validator), `backend/agent_py/agent/main.py` (SSE generator: session creation + per-message writes), `backend/agent_py/agent/observability.py` (counter + span registration if a registry lives there).
- **Wire**: none.
- **Dependencies**: none new. `asyncpg` already in the pool (`run_metadata.py`); `sqlalchemy` already in alembic.
- **Docs**: `FUTURE_AGENT.md` §2.1 supersession note; `backend/agent_py/README.md` env-var table; `AgentMemoryRoadmap.md` Slice 1 tick.
- **Tests**: `backend/agent_py/tests/test_chat_store.py` (create + record + retention), `backend/agent_py/tests/test_retention_chat.py` (cron with fake clock), extension to `test_smoke_run_state.py` (verify a chat run leaves rows in `agent.chat_messages`).
- **Rollout gates tracked in** `openspec/changes/agent-chat-history-storage-rollout/` (per the rollout-split convention).
