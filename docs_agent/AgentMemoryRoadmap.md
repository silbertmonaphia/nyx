# Agent Memory Roadmap

The slice plan that turns the three pain points in [`AgentMemoryDesign.md`](./AgentMemoryDesign.md) into shippable OpenSpec changes. For the long-term agent capability direction (P0–P2 across provider/tool/loop/state/observability/web-tools), see [`FUTURE_AGENT.md`](../FUTURE_AGENT.md) §2.1 — this roadmap supersedes that single-line entry with the actual split.

---

## Cross-cutting decisions (locked)

Recorded up front so every slice below inherits them without re-debating.

| Decision | Choice | Rationale |
|---|---|---|
| Storage location | Python agent writes to its own DB | Agent is the data processor; keeps the seam clean (`PYTHON_AGENT_URL` is the only boundary). |
| Schema namespace | `agent.*` (alongside `agent.runs`) | Existing alembic config already targets the `agent` schema; langGraph lives in `langgraph.*`. |
| Retention floor | **≥ 1 year** (`PYTHON_AGENT_CHAT_RETENTION_DAYS=365`) | User requirement; soft-delete + 7-day grace period before hard-delete (mirrors the run-retention pattern in `agent-stale-run-sweeper`). |
| GDPR posture | Right-to-erasure + data export + audit log | Non-negotiable at the ≥1-year floor. Lands in Slice 4 once all data surfaces are known. |
| Preference extraction | **Async**, fired after the `done` SSE frame, never inline | Avoids latency tax on the main stream; failures are warn-logged, retried next session. |
| History access pattern | **ChatGPT-style sidebar** (paginated 10-per-page, lazy-load, no injection into current planner prompt) | Replay ≠ context injection. Current conversation context only carries the live session's history. |
| Mem0 / external memory layer | **Not in v1** | First version is ~50 lines of custom extraction over `chat_user_prefs`. Upgrade to mem0 (library mode, 0 new pods) only if semantic recall becomes a real need. |

---

## The 4 slices

```
Slice 1: chat-history-storage        ── 纯数据层，0 业务逻辑
   │
   ├── Slice 2: chat-history-ui      ── 用户能看见/回放历史
   └── Slice 3: user-preferences     ── async 抽取 + planner 注入（与 2 并行）
            │
Slice 4: gdpr-compliance             ── 把所有用户数据出口/入口封死
```

Each slice produces an `../openspec/changes/agent-<slice>/` (impl) + a sibling `*-rollout/` per `../openspec/config.yaml`. Total: **8 OpenSpec changes** for the four slices (plus the follow-up `agent-chat-background-runs` and `agent-chat-fixes` changes that modified Slices 2/3 surfaces).

**All four impl slices are landed, reviewed, e2e-verified, and archived** (per-slice `Status` lines below). The four `*-rollout` siblings remain active — prod env flips + CI-green windows are rollout work, not dev work. Two follow-up changes also landed/archived: `agent-chat-background-runs` (2026-10-15) and `agent-chat-fixes` (2026-10-16).

---

### Slice 1 — `agent-chat-history-storage`

**Status: ✅ landed + archived** — `../openspec/changes/archive/2026-10-14-agent-chat-history-storage/` · commit `ab3f9e0` · rollout gates in `agent-chat-history-storage-rollout/`.

**Scope.** The data plane. Nothing user-visible.

- alembic migration: `agent.chat_sessions(id, user_id, title, started_at, last_active_at, ended_at, deleted_at)` + `agent.chat_messages(id, session_id, role, content, tool_calls jsonb, tokens, created_at)` with index on `(user_id, last_active_at desc) WHERE deleted_at IS NULL`.
- Python agent writes one row per session start; user messages are written at request entry, the assistant message at terminal `done` (synchronous writes before async extraction kicks off in Slice 3).
- Retention cron: `PYTHON_AGENT_CHAT_RETENTION_DAYS=365` (default), soft-delete + 7-day grace, hard-delete via SQL.
- **No new HTTP endpoints**, no frontend changes.

**Verification.**
- Unit: alembic up + down clean; retention cron unit-tested with a fake clock.
- E2E: full stack up → send a chat → `SELECT count(*) FROM agent.chat_messages m JOIN agent.chat_sessions s ON s.id = m.session_id WHERE s.user_id=…` = expected; backdate `last_active_at` past the retention window, run cron, rows gone.
- Backward compat: when `AGENT_ENABLED=false`, no rows written; today's single-turn path untouched.

**Files.**
- `backend/agent_py/migrations/versions/0004_chat_sessions_and_messages.py`
- `backend/agent_py/agent/chat_store.py` (new)
- `backend/agent_py/agent/retention.py` (extend existing)
- Tests: `backend/agent_py/tests/test_chat_store.py`, `test_retention_chat.py`

---

### Slice 2 — `agent-chat-history-ui`

**Status: ✅ landed + archived** — `../openspec/changes/archive/2026-10-14-agent-chat-history-ui/` · commit `996ed85` · rollout gates in `agent-chat-history-ui-rollout/`. As landed: the SPA reaches the agent via a Go proxy (`/api/chat/sessions*`; the upstream Python routes are `/agent/sessions*`), the sidebar is flag-gated (`VITE_CHAT_HISTORY_SIDEBAR_ENABLED`), and there is **no idle-based session splitting** — a session ends on explicit "+ New chat" only; echoing a live `run_id` resumes that run, while a new message on a live session continues it (fresh plan, no new `agent.runs` row).

**Post-landing follow-up: `agent-chat-background-runs`** (2026-10-15, `../openspec/changes/archive/2026-10-15-agent-chat-background-runs/`) — navigation no longer kills a run. `+ New chat`, a sidebar session switch, and dialog close now *detach* the SSE stream instead of aborting it (the connection is the run's liveness signal; aborting it cancels the agent server-side mid-flight). A detached run pulses on its rail row, locks the busy session's composer until its terminal frame (a second concurrent run on one session would interleave rows), then refreshes the rail and replays the completed turn. Cancel and deleting the active session remain the only aborts. Spec delta: `chat-history-sidebar` ("Click-to-replay", "Plus-New-chat button", new "Background runs survive navigation").

**Scope.** Make stored history visible and replayable.

- Endpoints (Python agent):
  - `GET /agent/sessions?limit=10&before=<cursor>` → paginated session list (latest first, cursor = `last_active_at` + `id`)
  - `GET /agent/sessions/{session_id}/messages` → message stream for one session
  - `POST /agent/sessions` → create empty session ("+ New chat")
  - `POST /agent/sessions/{session_id}/resume` → resume a historical session
  - `DELETE /agent/sessions/{session_id}` → user-initiated delete
- Session boundary signal: user clicks "+ New chat" only. As landed there is no idle-based splitting — an echoed `run_id` deliberately keeps its thread (`main.py:823-826`).
- Frontend (`frontend/src/features/chat/`):
  - `ChatSidebar.tsx` (new) — paginated list with IntersectionObserver-driven lazy load (polyfill already in `frontend/src/test/setup.js`)
  - `ChatPanel.tsx` refactor — clear current token stream on session switch, load historical messages, render
  - `chatStore` gains `currentSessionId`, `sessionList`, `loadMore`
  - "+ New chat" button in the header

**Verification.**
- Unit: endpoint handlers + pagination cursor logic + chatStore actions.
- E2E (Playwright `chat-history-sidebar.spec.ts`): open 3 sessions in one browser → reload → sidebar shows 3 with correct order → click the oldest → message stream renders verbatim → click "+ New chat" → fresh empty session.

**Files.**
- `backend/agent_py/agent/api/chat_sessions.py` (new)
- `frontend/src/features/chat/components/ChatSidebar.tsx`
- `frontend/src/features/chat/components/ChatPanel.tsx`
- `frontend/src/features/chat/store/chatStore.ts`
- Tests: `frontend/tests/e2e/chat-history-sidebar.spec.ts`, `backend/agent_py/tests/test_chat_sessions_api.py`

---

### Slice 3 — `agent-user-preferences`

**Status: ✅ landed + archived** — `../openspec/changes/archive/2026-10-14-agent-user-preferences/` · commit `44306ce` · rollout gates in `agent-user-preferences-rollout/`. As landed: rollback switch is `PYTHON_AGENT_PREFS_ENABLED` (extraction + injection together); extraction is bounded (key regex, value cap, per-turn cap) and never budget-charged; injection covers the planner **and** both executor answer paths (`llm` / `react`, added in `agent-chat-fixes`).

**Scope.** Agent remembers user-stated preferences across sessions.

- New table: `agent.chat_user_prefs(id, user_id, key, value jsonb, source_session_id, created_at, updated_at)` with unique `(user_id, key)` and index on `(user_id, updated_at desc)`.
- Extraction pipeline:
  - **Async fire-and-forget** after the streaming `done` frame; never blocks the response.
  - Optional lightweight gate (regex / tiny classifier) skips messages with no preference signal — first version may skip the gate for simplicity.
  - Output schema: `[{key, value, confidence}]`; upsert into `chat_user_prefs`.
- Prompt injection: load the user's prefs (top `PYTHON_AGENT_PREFS_MAX_INJECTED`, default 50, by `updated_at`) and prepend them to the planner system prompt **and** the executor's `llm` / `react` answer prompts (the executor half landed in `agent-chat-fixes`).
- **No frontend changes** in this slice.

**Verification.**
- Unit: extraction function with canned messages → expected key/value list; planner prompt builder includes the prefs; async task survives errors (warn + continue).
- E2E (Playwright `chat-user-prefs.spec.ts`): send "我喜欢鱿鱼丝" → wait for extraction to complete → open a new session via "+ New chat" → ask "推荐零食" → response references 鱿鱼丝.
- Failure isolation: if extraction LLM call fails, the chat still streams to `done`; only the `chat_user_prefs` write is missing (no user-visible error).

**Files.**
- `backend/agent_py/agent/prefs.py` (new)
- `backend/agent_py/agent/planner.py` (extend to inject prefs)
- alembic: `<new>_chat_user_prefs.py`
- Tests: `backend/agent_py/tests/test_prefs.py`, `test_prefs_injection.py`, `frontend/tests/e2e/chat-user-prefs.spec.ts`

---

### Slice 4 — `agent-gdpr-compliance`

**Status: ✅ landed + archived** — `../openspec/changes/archive/2026-10-14-agent-gdpr-compliance/` · commit `3814dac` · rollout gates in `agent-gdpr-compliance-rollout/`. As landed: checkpoint-thread erasure/export covers the **deduped union of run ids ∪ session ids** (continuation turns are session-keyed); export additionally carries the subject's own `audit_entries`; audit retention default 1095d with a 365d floor; the Playwright leg is replaced by live curl smokes + unit coverage (operator-only endpoints, transcript in the change's `e2e-evidence.md`).

**Scope.** Seal all user-data exits; right-to-erasure end-to-end.

- Endpoints (Python agent, **owner-scoped via `X-Nyx-User-Id`**, no admin override):
  - `GET /agent/me/export` → JSON dump: `{user_id, exported_at, sessions, messages, prefs, runs, checkpoint_thread_ids, audit_entries}` (checkpoint content requires deserialization — out of scope, ids only)
  - `DELETE /agent/me` → physical delete in this order: `chat_messages` → `chat_sessions` → `chat_user_prefs` (one transaction), then `agent.runs` (deleted ids), then langGraph checkpoint threads (deduped run-id ∪ session-id union), then the audit log row
- Audit log: new table `agent.data_audit_log(id, user_id, action, payload_summary, requested_at)` (append-only, retention 3 years — separate from the 1-year chat retention; required for compliance evidence).
- Documented in `docs/agent-deployment.md`: data inventory (where user data lives), retention windows, export/erase SLA, and the playbook for handling a real erasure request.

**Verification.**
- Unit: export snapshot completeness; delete cascades through every table; audit log written; idempotent (double-delete safe).
- E2E as landed: live curl smokes against the side stack (register a user → run sessions → `GET /agent/me/export` → assert messages/prefs → `DELETE /agent/me` → re-export empty → checkpoint threads gone). The Playwright leg was dropped; the transcript lives in the change's `e2e-evidence.md`.
- Documentation review: `../docs/agent-deployment.md` updated; `SECURITY.md` cross-link added.

**Files.**
- `backend/agent_py/agent/api/gdpr.py` (new)
- `backend/agent_py/agent/audit.py` (new)
- alembic: `backend/agent_py/migrations/versions/0006_data_audit_log.py`
- `../docs/agent-deployment.md` (extend)
- `SECURITY.md` (cross-link)
- Tests: `backend/agent_py/tests/test_gdpr_endpoints.py`, `test_gdpr_stores.py`

---

## Why this split (not one mega-slice)

| Principle | How the split honors it |
|---|---|
| **Dependency order** | Slice 1 is the unmovable foundation; Slices 2 + 3 are two independent product capabilities on top; Slice 4 seals everything. |
| **Solo archive** | Each slice has clean design/tasks/e2e/archive boundaries — no "one task blocks archive" trap. |
| **Solo rollback** | Slice 3 broken? Turn off `PYTHON_AGENT_PREFS_ENABLED=false`. Slice 2 broken? Hide the sidebar behind a flag. No flag-day coupling. |
| **Solo rollout** | Each impl slice has its own `*-rollout` sibling for the prod env-flip + 1-week CI-green window. 8 OpenSpec changes total for the four slices (plus the two follow-up changes), but each is small (100–300 lines). |
| **E2E per slice** | Slices 1/2/3 own Playwright specs (sidebar list, prefs recall); Slice 4 (operator-only endpoints) uses live curl smokes + unit coverage instead. No shared "huge e2e" surface. |

---

## Out of scope (deferred; revisit only if real need surfaces)

- **Cross-session semantic recall** (top-K over past sessions at session start) — was in `FUTURE_AGENT.md` §2.1. Not needed for v1; if it becomes one, upgrade prefs extraction to mem0 library mode (0 new pods) and add a recall step in Slice 3.
- **Self-reflection / CRAG** — explicitly out per `FUTURE_AGENT.md` §2.2.
- **mem0 self-hosted server mode** — deferred. Library mode is the only escape valve if recall matters later.

---

## References

- [`AgentMemoryDesign.md`](./AgentMemoryDesign.md) — the three pain points this roadmap addresses.
- [`FUTURE_AGENT.md`](../FUTURE_AGENT.md) §2.1 — original (single-line) entry this roadmap supersedes.
- [`FUTURE_AGENT.md`](../FUTURE_AGENT.md) §1.1 — precedent for the retention cron + grace-period pattern (`agent-stale-run-sweeper`).
- `../openspec/config.yaml` — rollout-split convention; each impl slice gets a `*-rollout` sibling.
- `../openspec/changes/agent-stale-run-sweeper-rollout/tasks.md` — model for the rollout-sibling task shape.
