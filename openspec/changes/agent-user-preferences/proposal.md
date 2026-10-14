# Proposal

## Why

Pain point 3 from `AgentMemoryDesign.md`: users report the agent treats every conversation like a stranger — it doesn't remember stated characteristics ("我喜欢鱿鱼丝，喜欢ACG"). Slice 1 gave history a durable home; Slice 2 made it visible; this slice makes the agent *use* it: extract stable user preferences from each turn (async, after the reply — never delaying the stream) and inject the accumulated preference set into the planner's system prompt so plans and answers account for who the user is. Per `AgentMemoryRoadmap.md`, this is the memory capability's core; cross-session *semantic recall* stays out of scope (deferred) — this is the lightweight key/value version the roadmap locked, with no external memory service (no mem0).

## What Changes

- New table `agent.chat_user_prefs(id bigserial, user_id bigint, key text, value jsonb, source_session_id uuid, created_at timestamptz, updated_at timestamptz)` with a UNIQUE index on `(user_id, key)` — upsert semantics per key; plus a `(user_id, updated_at DESC)` index for the injection read. Alembic `0005`. Rows are collected when their `source_session_id` session's retention window lapses (same window as chat history — a deleted session's derived memory does not outlive it; the hard-delete path deletes matching prefs rows).
- New `backend/agent_py/agent/prefs.py`:
  - `extract_preferences(messages, *, settings, client) -> list[Pref]` — one small LLM call with a JSON-output prompt; returns `[{key, value, confidence}]`; Pydantic-validated; malformed output logs and returns `[]`.
  - `looks_like_preference(text) -> bool` — the cheap heuristic gate (EN + ZH first-person preference markers, e.g. "I like/love/prefer/am into", "我喜欢/我爱/偏好/是一个…迷"); skips the LLM call for messages with no signal.
  - `upsert_preferences(store, user_id, prefs, source_session_id)` — upsert via the unique `(user_id, key)` index.
  - `load_preferences(store, user_id, *, limit) -> list[Pref]` — top-N by `updated_at DESC` for injection.
  - `prefs_block(prefs) -> str` — renders the planner prompt block; returns `""` for an empty list so the prompt stays byte-identical (the injection parity contract).
- New `PrefsStore` methods in `backend/agent_py/agent/chat_store.py` (or a sibling `prefs_store.py` reusing the same pool): `upsert_pref`, `load_prefs`, `delete_prefs_for_session`, `delete_prefs_for_user` (the last two serve slice-4 erasure + retention).
- Wiring in `backend/agent_py/agent/main.py`: after the assistant message is recorded (post-`done`), fire an `asyncio.create_task` that runs the gate → extraction → upsert. Fire-and-forget: the task never touches the SSE stream; failures are warn-logged. The extraction LLM call does NOT consume the per-user chat budget (it's an internal operation bounded to one call per preference-bearing turn).
- Planner injection in `backend/agent_py/agent/planner.py`: `_build_planner_messages` appends `prefs_block(prefs)` to the system prompt when prefs exist. With no prefs (or `PYTHON_AGENT_PREFS_ENABLED=false`), the prompt is byte-identical to the pre-injection form.
- New settings: `PYTHON_AGENT_PREFS_ENABLED` (default true — kill-switch for extraction AND injection), `PYTHON_AGENT_PREFS_MAX_INJECTED` (default 50; MUST be ≥ 0). No frontend changes anywhere — prefs are invisible plumbing.
- **Wire**: unchanged. No new SSE frames, no new HTTP endpoints (the prefs table is read server-side only; slice 4's export/erase owns the user-facing surface).

## Capabilities

### New Capabilities

- `agent-user-prefs`: the `agent.chat_user_prefs` table shape, the extraction pipeline (gate → LLM → upsert, async and failure-isolated), the planner prompt injection (with the empty-set parity contract), and the lifecycle coupling to chat-history retention.

### Modified Capabilities

- `agent-python-service`: env-var list gains `PYTHON_AGENT_PREFS_ENABLED` (default true) and `PYTHON_AGENT_PREFS_MAX_INJECTED` (default 50, must be ≥ 0) + the startup validator.
- `agent-chat-history-store`: the retention story extends from "sessions + messages" to "sessions + messages + their derived prefs" — hard-delete of a session also deletes `chat_user_prefs` rows whose `source_session_id` is that session.

## Impact

- **Code**: `backend/agent_py/agent/prefs.py` (new), `backend/agent_py/agent/chat_store.py` (PrefsStore methods), `backend/agent_py/agent/planner.py` (injection site at `_build_planner_messages`), `backend/agent_py/agent/main.py` (async extraction task after the `done` path), `backend/agent_py/agent/config.py` (2 settings + validator), `backend/agent_py/migrations/versions/0005_chat_user_prefs.py` (new), `backend/agent_py/agent/retention.py` (prefs cleanup rides the chat hard-delete).
- **Wire**: none.
- **Dependencies**: none new (the extraction call reuses the existing `langchain-openai` client stack).
- **Docs**: `backend/agent_py/.env.example` (2 new vars), `backend/agent_py/README.md`, `FUTURE_AGENT.md` §2.1 (prefs bullet tick), `AgentMemoryRoadmap.md` Slice 3 tick.
- **Tests**: `backend/agent_py/tests/test_prefs.py` (gate heuristic, extraction parse/validation with a stubbed client, upsert semantics, injection block rendering + empty-set parity, async failure isolation), retention coupling test (session hard-delete removes its prefs), `frontend/tests/e2e/chat-user-prefs.spec.ts` (live-stack recall leg, model-nondeterminism tolerant per the rollout sibling's §2.1).
- **Rollout gates tracked in** `openspec/changes/agent-user-preferences-rollout/` (per the rollout-split convention).
