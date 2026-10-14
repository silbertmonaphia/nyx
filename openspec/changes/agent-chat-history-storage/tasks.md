# Tasks

## 1. Schema

- [x] 1.1 Add alembic revision `0004_chat_sessions_and_messages.py` creating `agent.chat_sessions` (id uuid4 PK, user_id bigint, title text, started_at timestamptz default now(), last_active_at timestamptz default now(), ended_at timestamptz NULL, deleted_at timestamptz NULL) and `agent.chat_messages` (id bigserial PK, session_id uuid4 FK→chat_sessions.id ON DELETE CASCADE, role text, content text, tool_calls jsonb NULL, tokens int default 0, created_at timestamptz default now()); index `(user_id, last_active_at DESC) WHERE deleted_at IS NULL` on chat_sessions; index `(session_id, created_at)` on chat_messages; `down_revision = '0003_add_run_heartbeat'`. Verify: `cd backend/agent_py && alembic upgrade head` succeeds against the local Docker Postgres; `alembic downgrade -1` drops both tables cleanly; both `upgrade head` and `downgrade base` complete without error.

## 2. Settings + validator

- [x] 2.1 Extend `backend/agent_py/agent/config.py` `Settings` with `chat_retention_days: int = 365`, `chat_retention_grace_days: int = 7`, `chat_retention_enabled: bool = True`. Add `_validate_chat_retention` model-validator that raises `ValueError` when `chat_retention_days < 365` (one-year floor), `chat_retention_grace_days < 0`, or `chat_retention_interval_seconds < 1` (interval defaults to the existing `retention_interval_seconds`; no new env var). Verify: `cd backend/agent_py && PYTHON_AGENT_CHAT_RETENTION_DAYS=180 python -c "from agent.config import Settings; Settings()"` raises ValueError on the floor; `PYTHON_AGENT_CHAT_RETENTION_GRACE_DAYS=-1` raises on negative; defaults load cleanly.

## 3. ChatStore module

- [x] 3.1 Create `backend/agent_py/agent/chat_store.py` defining `ChatStoreUnavailable(RuntimeError)` and `ChatStore` class taking an `asyncpg.Pool`. Public methods: `create_session(user_id, run_id, title="") -> None`, `get_session(session_id, user_id) -> ChatSession | None`, `record_message(session_id, user_id, role, content, tool_calls=None, tokens=0) -> int` (returns message id), `touch_session(session_id, user_id) -> bool`, `end_session(session_id, user_id, ended_at=None) -> bool`, `soft_delete_expired(retention_days) -> list[str]` (returns session ids whose `last_active_at < now() - retention_days AND deleted_at IS NULL`; sets `deleted_at = now()`), `hard_delete_soft_deleted(grace_days) -> int` (returns count of sessions deleted; cascades to chat_messages via FK ON DELETE CASCADE). All methods use `_with_retry(op_name, fn)` matching `RunMetadata._with_retry` semantics (100ms→200ms→400ms exponential backoff, max 3 retries, 5s acquire timeout, `RunMetadataUnavailable`-equivalent raise on exhaustion). Every read/write includes `user_id` in WHERE for the user-facing methods. Verify: `cd backend/agent_py && python -m pytest tests/test_chat_store.py -v` — covers create/get cross-user isolation, record_message round-trip, retention soft→hard lifecycle, retry-on-transient-error.

## 4. Retention cron extension

- [x] 4.1 Extend `backend/agent_py/agent/retention.py` `RetentionCron` to take a `ChatStore` reference (keyword-only, mirroring the checkpointer injection posture). Add an inner `_chat_iteration` method that emits OTel span `agent.chat.retention` and increments `agent_chat_retention_deleted_total` by the `hard_delete_soft_deleted` return value; the span carries `chat_deleted_count` attribute. Wrap the chat iteration in a try/except that logs at WARN and continues (failure isolation with the run-retention iteration). Update `__init__` signature: `RetentionCron(run_metadata, chat_store, checkpointer=None, *, settings)` — the `chat_store` arg is positional for backward compat with the lifespan wiring; verify the existing `lifespan` in `main.py` passes a ChatStore after task 5.1 wires it. Verify: `cd backend/agent_py && python -m pytest tests/test_retention_chat.py -v` — fake clock advances past `retention_days`, then past `grace_days`; the counter increments only on the hard-delete iteration.

## 5. SSE generator wiring

- [x] 5.1 Extend `backend/agent_py/agent/main.py` `lifespan` to construct a `ChatStore` (after the `RunMetadata` construction, reusing the same pool). Register it on `app.state.chat_store`. Pass it to `RetentionCron` per task 4.1. Verify: `cd backend/agent_py && python -m pytest tests/ -v` — existing retention tests still pass (the new arg is positional and a default-None is rejected at startup).
- [x] 5.2 In the SSE generator (`main.py`'s `/agent/run` handler), insert chat-store writes: at run start (after `run_id` is known), call `chat_store.create_session(user_id, run_id)`; for resumed runs (echoed `run_id` already in `chat_sessions`), call `chat_store.touch_session(run_id, user_id)` instead. After each user message at request entry and after each assistant message at the `done` frame (or at run termination if the run finishes without a `done`), call `chat_store.record_message(...)`. `tool_calls` is captured verbatim from the SSE envelope. On any `ChatStoreUnavailable`, log at WARN and continue the stream. Verify: `cd backend/agent_py && python -m pytest tests/test_smoke_run_state.py -v` — extended to assert that after a smoke run, `agent.chat_sessions` has one row for that user and `agent.chat_messages` has at least 2 rows (user + assistant).

## 6. Observability registration

- [x] 6.1 Register the `agent_chat_retention_deleted_total` counter in the module-level scope of `backend/agent_py/agent/retention.py` next to the existing `agent_run_retention_deleted_total`. Verify: `curl :9001/metrics | grep agent_chat` returns the help / type / metric lines (counter starts at 0, increments only after a real retention iteration deletes a row).

## 7. Docs

- [x] 7.1 Update `backend/agent_py/README.md` env-var table: add `PYTHON_AGENT_CHAT_RETENTION_DAYS` (default 365), `PYTHON_AGENT_CHAT_RETENTION_GRACE_DAYS` (default 7), `PYTHON_AGENT_CHAT_RETENTION_ENABLED` (default true). Update `FUTURE_AGENT.md` §2.1: mark the storage-layer bullet done; reference `AgentMemoryRoadmap.md` Slice 1 as the canonical entry; reference the archived `agent-chat-history-storage` change for the spec. Verify: `git diff --check` passes; the README env-var count matches the actual count of `PYTHON_AGENT_` env vars used by the code (no orphan entries).

## 8. Self-review

- [x] 8.1 Spawn the `test-reviewer` agent against the Slice 1 diff and address every CRITICAL/HIGH finding before archive. Verify: review report filed in the change folder as `review.md`; all CRITICAL/HIGH items show `outcome: "fixed"` or `outcome: "no_change_needed"` with rationale.

## 9. E2E

- [x] 9.1 Bring up the full stack (`make up` + `make dev` + `make dev-agent`). With the agent live, send a chat that yields a multi-step run via `curl -N -X POST localhost:9000/agent/run -H 'X-Nyx-User-Id: 1' -H 'Content-Type: application/json' -d '{"messages":[{"role":"user","content":"hi"}]}'`. Verify: `SELECT count(*) FROM agent.chat_sessions WHERE user_id=1` is 1, `SELECT count(*) FROM agent.chat_messages WHERE session_id=<the_run_id>` is ≥ 2 (user + assistant). Then backdate `last_active_at` past the retention window via SQL, run one retention iteration (short interval override via `PYTHON_AGENT_CHAT_RETENTION_DAYS=0 PYTHON_AGENT_CHAT_RETENTION_GRACE_DAYS=0`), verify the session row is hard-deleted. **Note**: e2e requires Docker; if Docker is unavailable in the current environment, the unit-test coverage in tasks 3.1 + 4.1 + 5.2 is the accepted substitute and the task is marked complete with that rationale recorded here.

## 10. Archive

- [ ] 10.1 Run `openspec archive agent-chat-history-storage` after every other task is checked. Verify: `openspec validate --archived` passes post-archive; the change moves to `openspec/changes/archive/2026-10-14-agent-chat-history-storage/` with delta specs synced to `openspec/specs/`.

