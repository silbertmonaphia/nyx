# Tasks

## 1. Go proxy layer

- [ ] 1.1 Add `RunID *string` (`json:"run_id,omitempty"`) to `ChatRequest` in `backend/internal/chat/model.go`; verify the existing `DisallowUnknownFields` decode accepts it and the proxied body carries it unchanged (regression test in `handler_test.go`); omitted field stays omitted (`omitempty`). Verify: `cd backend && SKIP_CONTAINERS=true go test ./internal/chat/...`.
- [ ] 1.2 Add the five session REST proxy routes (`GET /api/chat/sessions`, `GET /api/chat/sessions/{id}/messages`, `POST /api/chat/sessions`, `POST /api/chat/sessions/{id}/resume`, `DELETE /api/chat/sessions/{id}`) in a new `backend/internal/chat/sessions_proxy.go` registered inside `RegisterChatRoute`'s route group (auth middleware inherited; no SSE). Forward to `PYTHON_AGENT_URL/agent/sessions...` with `X-Nyx-User-Id` + `Authorization`; forward status/body/query verbatim; 502 on unreachable, 503 when `AGENT_ENABLED=false`, safe envelope details via `api.ClassifyAndLog`. Verify: `cd backend && go test ./internal/chat/... -run Sessions` + `golangci-lint run --timeout=5m`.

## 2. Python settings + validator

- [ ] 2.1 Extend `backend/agent_py/agent/config.py` `Settings` with `sessions_page_size: int = 10`, `session_idle_min: int = 30`. Add validator that raises `ValueError` when `sessions_page_size < 1 or > 50`, `session_idle_min < 1`. Verify: defaults load; `PYTHON_AGENT_SESSIONS_PAGE_SIZE=0` and `=51` both raise; `PYTHON_AGENT_SESSION_IDLE_MIN=0` raises.

## 3. Python endpoints

- [ ] 3.1 Create `backend/agent_py/agent/api/chat_sessions.py` exposing:
  - `GET /agent/sessions?limit=&before=` — owner-scoped paginated list with cursor encoding `(last_active_at, id)` as base64url `<unix-microseconds>_<uuid>`; response carries `sessions[]` (each with `message_count` via a single `LEFT JOIN ... GROUP BY` to avoid N+1) and `next_cursor`
  - `GET /agent/sessions/{session_id}/messages` — owner-scoped, soft-deleted returns 404, message replay ordered by `created_at ASC, id ASC`
  - `POST /agent/sessions` — creates an empty session for the requesting user, returns `{session_id, started_at}`
  - `POST /agent/sessions/{session_id}/resume` — refreshes `last_active_at`, returns `{session_id, resumed_at}`
  - `DELETE /agent/sessions/{session_id}` — soft-deletes (`deleted_at = now()`); messages stay until retention
  - All endpoints enforce `X-Nyx-User-Id` (400 when missing / non-integer) and emit per-request OTel span attributes `endpoint`, `user_id`, `result_count` (where applicable). Verify: `python -m pytest tests/test_chat_sessions_api.py -v` — pagination cursor stability, cross-user 404, soft-delete + replay, N+1 verification on message_count.

## 4. main.py router wiring

- [ ] 4.1 Register the new router on the FastAPI app in `backend/agent_py/agent/main.py` lifespan. The chat_store is already on `app.state` from Slice 1. Verify: existing tests still pass.

## 5. Frontend services

- [ ] 5.1 Extend `frontend/src/features/chat/services/chatService.ts` (or new `chatSessionsService.ts`) with `listSessions({limit, before})`, `getMessages(sessionId)`, `createSession()`, `resumeSession(sessionId)`, `deleteSession(sessionId)`. The Go chat proxy chain handles the user id injection; the SPA just sends the requests and reads the responses. Also update the chat send path to echo `run_id` in the POST body when an active session exists.

## 6. Frontend state

- [ ] 6.1 Extend `frontend/src/features/chat/store/chatStore.ts` with `currentSessionId`, `sessionList`, `sessionListNextCursor`, `isFetchingMore`, `loadMore()`, `setCurrentSession(id)`, `clearActiveSession()`. Persist `currentSessionId` to `sessionStorage`. Implement single-flight `loadMore` (drops the second call while the first is in flight).

## 7. Frontend components

- [ ] 7.1 Create `frontend/src/features/chat/components/ChatSidebar.tsx` rendering the session list with an IntersectionObserver-driven sentinel for lazy-load. Each row is clickable; clicking the active session is a no-op. Hidden on viewports narrower than 768px (replaced by a drawer toggle in the chat header).
- [ ] 7.2 Refactor `frontend/src/features/chat/ChatPanel.tsx` to: (a) call `setCurrentSession` on session switch and load messages via `getMessages`; (b) clear the live stream on switch; (c) render the `+ New chat` button in the header that POSTs `/api/chat/sessions` and resets the active state.
- [ ] 7.3 Add a mobile drawer toggle (`md:hidden`) in the chat header that opens/closes the sidebar on narrow viewports; auto-close the drawer on session click.

## 8. Self-review

- [ ] 8.1 Spawn the `test-reviewer` agent against the Slice 2 diff and address every CRITICAL/HIGH finding before archive. Verify: review report filed in the change folder as `review.md`.

## 9. E2E

- [ ] 9.1 Bring up the full stack. Run `cd frontend && npm run test:e2e -- tests/e2e/chat-history-sidebar.spec.ts`. Verify green; if red, record the failure verbatim. **Note**: e2e requires Docker; if Docker is unavailable in the current environment, the unit + Playwright stub-level coverage is the accepted substitute and the task is marked complete with that rationale recorded here.

## 10. Archive

- [ ] 10.1 Run `openspec archive agent-chat-history-ui --yes` after every other task is checked. Verify: `openspec validate --archived` passes post-archive; the change moves to `openspec/changes/archive/2026-10-14-agent-chat-history-ui/` with delta specs synced to `openspec/specs/`.
