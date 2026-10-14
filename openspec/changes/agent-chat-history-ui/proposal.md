# Proposal

## Why

Slice 1 (`agent-chat-history-storage`, archived 2026-10-14) wrote every chat to `agent.chat_sessions` + `agent.chat_messages`, but the user can't see anything — the SPA only renders the live stream. Operators have analytics (they can `SELECT` from the tables directly), but the user-facing pain point 2 from `AgentMemoryDesign.md` ("关掉窗口之后就找不到之前比如3天前那个对话了") is unresolved. This slice makes stored history visible and replayable: a ChatGPT-style sidebar lists past sessions with lazy-load, clicking one loads its messages into `ChatPanel`, a "+ New chat" button starts a fresh session, and per-session delete + resume round out the surface. The wire / agent run loop / SSE contract are unchanged — this is purely a read-side UI.

## What Changes

- Go chat proxy (`backend/internal/chat/`):
  - `ChatRequest` gains an optional `RunID *string` field (`json:"run_id,omitempty"`) so the SPA can echo a session id through the proxy (today the field 400s via `DisallowUnknownFields`). The proxy forwards it unchanged; Python owns validation.
  - New authenticated REST routes under `/api/chat/sessions` mirroring the agent's session endpoints 1:1 (`GET` list, `GET .../{id}/messages`, `POST` create, `POST .../{id}/resume`, `DELETE .../{id}`) — plain JSON proxy to `PYTHON_AGENT_URL/agent/sessions...` with `X-Nyx-User-Id` (from `reqctx`) + `Authorization` passthrough. Agent unreachable → 502; `AGENT_ENABLED=false` → 503; both with the standard safe error envelope.
- New endpoints in `backend/agent_py/agent/api/chat_sessions.py`:
  - `GET /agent/sessions?limit=10&before=<cursor>` → paginated session list (latest first); cursor is `last_active_at + id` for stable pagination across same-second updates
  - `GET /agent/sessions/{session_id}/messages` → message stream for one session
  - `POST /agent/sessions` → create an empty session ("+ New chat"); returns `{session_id, created_at}`
  - `POST /agent/sessions/{session_id}/resume` → mark the session as the active one for the next `/agent/run` (echoes back via the existing `run_id` resume path)
  - `DELETE /agent/sessions/{session_id}` → user-initiated soft-delete; FK CASCADE handles messages
  - All endpoints are `X-Nyx-User-Id`-scoped (no admin override); cross-user requests 404
- Frontend (`frontend/src/features/chat/`):
  - `ChatSidebar.tsx` (new) — paginated list with IntersectionObserver-driven lazy load (polyfill already in `frontend/test/setup.js`); each row shows title + relative timestamp + unread dot if applicable
  - `ChatPanel.tsx` refactor — on session switch, clear the live token stream, load the historical messages via `GET /agent/sessions/{id}/messages`, render; restore the live stream on the next user-sent message
  - `chatStore` gains `currentSessionId`, `sessionList`, `loadMore`, `setCurrentSession(id)`; persists to `sessionStorage` so a tab reload restores the current session
  - Header `+ New chat` button → `POST /agent/sessions` → switches the active session id
- Session boundary signals: the existing "+ New chat" button or 30-min idle (`PYTHON_AGENT_SESSION_IDLE_MIN`) opens a new session; the SSE generator in `main.py` already records the current `run_id` to the active chat_sessions row on each run start (Slice 1), so no main.py changes are needed beyond a small "session-resume" hint
- **No new SSE frames**, **no new wire fields**, **no changes to the chat-history-storage slice's data plane**
- Pagination: cursor-based (`before=<last_active_at>_<id>`), 10 per page by default, max 50 per request; never returns soft-deleted rows

## Capabilities

### New Capabilities

- `agent-chat-sessions-api`: HTTP read surface over `agent.chat_sessions` and `agent.chat_messages`. Cursor-paginated list, per-session message replay, create / resume / delete operations. Owner-scoped via `X-Nyx-User-Id`; cross-user requests 404.
- `chat-history-sidebar`: SPA sidebar component with paginated session list, lazy-load on scroll, click-to-load history. Paired with `ChatPanel`'s session-switch behavior and the new `chatStore` actions. Mobile-aware (drawer toggle on narrow viewports).

### Modified Capabilities

- `chat-agent-proxy`: gains the optional `run_id` passthrough on `POST /api/chat` (new `RunID` field on the wire struct) and the five authenticated session REST proxy routes under `/api/chat/sessions`.
- `agent-python-service`: gains `PYTHON_AGENT_SESSION_IDLE_MIN` (default 30) and `PYTHON_AGENT_SESSIONS_PAGE_SIZE` (default 10) env vars; the lifespan wires the chat-sessions router to the FastAPI app.

## Impact

- **Code**:
  - `backend/agent_py/agent/api/chat_sessions.py` (new) — endpoints + pagination cursor encoding
  - `backend/agent_py/agent/main.py` — register the router; the existing lifespan already has `chat_store` on `app.state`
  - `frontend/src/features/chat/components/ChatSidebar.tsx` (new)
  - `frontend/src/features/chat/ChatPanel.tsx` (extend)
  - `frontend/src/features/chat/store/chatStore.ts` (extend)
  - `frontend/src/features/chat/services/chatService.ts` (extend) — methods for sessions list / messages / create / resume / delete
- **Wire**: unchanged on the agent SSE side. The Go `chat-agent-proxy` reads the `run_id` from the request body (no change). Endpoints above are REST (not SSE).
- **Dependencies**: none new. asyncpg + pydantic already in the agent; React + Zustand + IntersectionObserver polyfill already in the SPA.
- **Docs**: `backend/agent_py/README.md` (new endpoints), `frontend/src/features/chat/README.md` if it exists (sidebar usage), `AgentMemoryRoadmap.md` Slice 2 tick.
- **Tests**:
  - `backend/agent_py/tests/test_chat_sessions_api.py` (new) — pagination cursor stability, cross-user 404, soft-delete + cascade, resume-of-deleted-session returns 404
  - `frontend/tests/e2e/chat-history-sidebar.spec.ts` (new) — open 3 sessions, reload, sidebar shows 3, click oldest, click `+ New chat`, fresh session
- **Rollout gates tracked in** `openspec/changes/agent-chat-history-ui-rollout/` (per the rollout-split convention).
