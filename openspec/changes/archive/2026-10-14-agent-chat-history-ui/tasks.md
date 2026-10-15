# Tasks

## 1. Go proxy layer

- [x] 1.1 Add `RunID *string` (`json:"run_id,omitempty"`) to `ChatRequest` in `backend/internal/chat/model.go`; verify the existing `DisallowUnknownFields` decode accepts it and the proxied body carries it unchanged (regression test in `handler_test.go`); omitted field stays omitted (`omitempty`). Verify: `cd backend && SKIP_CONTAINERS=true go test ./internal/chat/...`.
- [x] 1.2 Add the five session REST proxy routes (`GET /api/chat/sessions`, `GET /api/chat/sessions/{id}/messages`, `POST /api/chat/sessions`, `POST /api/chat/sessions/{id}/resume`, `DELETE /api/chat/sessions/{id}`) in a new `backend/internal/chat/sessions_proxy.go` registered inside `RegisterChatRoute`'s route group (auth middleware inherited; no SSE). Forward to `PYTHON_AGENT_URL/agent/sessions...` with `X-Nyx-User-Id` + `Authorization`; forward status/body/query verbatim; 502 on unreachable, 503 when `AGENT_ENABLED=false`, safe envelope details via `api.ClassifyAndLog`. Verify: `cd backend && go test ./internal/chat/... -run Sessions` + `golangci-lint run --timeout=5m`.

## 2. Python settings + validator

- [x] 2.1 Extend `backend/agent_py/agent/config.py` `Settings` with `sessions_page_size: int = 10`. Add validator that raises `ValueError` when `sessions_page_size < 1 or > 50`. Verify: defaults load; `PYTHON_AGENT_SESSIONS_PAGE_SIZE=0` and `=51` both raise. (The planned `session_idle_min` setting was removed in the review round — see task 10.2; an echoed session id always continues, there is no idle chopping.)

## 3. Python endpoints

- [x] 3.1 Create `backend/agent_py/agent/api/chat_sessions.py` exposing:
  - `GET /agent/sessions?limit=&before=` — owner-scoped paginated list with cursor encoding `(last_active_at, id)` as base64url `<unix-microseconds>_<uuid>`; response carries `sessions[]` (each with `message_count` via a single `LEFT JOIN ... GROUP BY` to avoid N+1) and `next_cursor`
  - `GET /agent/sessions/{session_id}/messages` — owner-scoped, soft-deleted returns 404, message replay ordered by `created_at ASC, id ASC`
  - `POST /agent/sessions` — creates an empty session for the requesting user, returns `{session_id, started_at}`
  - `POST /agent/sessions/{session_id}/resume` — refreshes `last_active_at`, returns `{session_id, resumed_at}`
  - `DELETE /agent/sessions/{session_id}` — soft-deletes (`deleted_at = now()`); messages stay until retention
  - All endpoints enforce `X-Nyx-User-Id` (400 when missing / non-integer) and emit per-request OTel span attributes `endpoint`, `user_id`, `result_count` (where applicable). Verify: `python -m pytest tests/test_chat_sessions_api.py -v` — pagination cursor stability, cross-user 404, soft-delete + replay, N+1 verification on message_count.

## 4. main.py router wiring

- [x] 4.1 Register the new router on the FastAPI app in `backend/agent_py/agent/main.py` lifespan. The chat_store is already on `app.state` from Slice 1. Verify: existing tests still pass.

## 5. Frontend services

- [x] 5.1 Extend `frontend/src/features/chat/services/chatService.ts` (or new `chatSessionsService.ts`) with `listSessions({limit, before})`, `getMessages(sessionId)`, `createSession()`, `resumeSession(sessionId)`, `deleteSession(sessionId)`. The Go chat proxy chain handles the user id injection; the SPA just sends the requests and reads the responses. Also update the chat send path to echo `run_id` in the POST body when an active session exists.

## 6. Frontend state

- [x] 6.1 Extend `frontend/src/features/chat/store/chatStore.ts` with `currentSessionId`, `sessionList`, `sessionListNextCursor`, `isFetchingMore`, `loadMore()`, `setCurrentSession(id)`, `clearActiveSession()`. Persist `currentSessionId` to `sessionStorage`. Implement single-flight `loadMore` (drops the second call while the first is in flight).

## 7. Frontend components

- [x] 7.1 Create `frontend/src/features/chat/components/ChatSidebar.tsx` rendering the session list with an IntersectionObserver-driven sentinel for lazy-load. Each row is clickable; clicking the active session is a no-op. Hidden on viewports narrower than 768px (replaced by a drawer toggle in the chat header).
- [x] 7.2 Refactor `frontend/src/features/chat/components/ChatPanel.tsx` to: (a) call `setCurrentSession` on session switch and load messages via `getMessages`; (b) clear the live stream on switch; (c) render the `+ New chat` button in the header that POSTs `/api/chat/sessions` and resets the active state.
- [x] 7.3 Add a mobile drawer toggle (`md:hidden`) in the chat header that opens/closes the sidebar on narrow viewports; auto-close the drawer on session click.

## 8. Self-review

- [x] 8.1 Spawn the `test-reviewer` agent against the Slice 2 diff and address every CRITICAL/HIGH finding before archive. Verify: review report filed in the change folder as `review.md`. **Done 2026-10-14** — 0 CRITICAL / 2 HIGH / 4 MEDIUM / 8 LOW; findings + fixes recorded in `review.md`. HIGH-1 (destructive fixtures vs shared DB) fixed via `tests/conftest.py` guard (refuses any DSN whose database isn't `nyx_test`) + all runs moved to the disposable `nyx_test` DB. HIGH-2 (continuation code unverified) fixed via `tests/test_session_continuation.py` (4 tests) + the full suite + the e2e re-run recorded in 9.1.

## 9. E2E

- [x] 9.1 Bring up the full stack. Run `cd frontend && npm run test:e2e -- tests/e2e/chat-history-sidebar.spec.ts`. Verify green; if red, record the failure verbatim. **Done 2026-10-14** — side stack (`scripts/e2e-side-stack.sh`: :8180/:9100/:5273, operator stack untouched) + vLLM. Serial (CI-mode) full suite: **26 passed** — includes the 6/6 sidebar spec and the full chat-rag suite. Full evidence in `e2e-evidence.md`; the parallel-local flake on live-LLM tests is the repo's documented shared-GPU contention (CI runs `workers: 1`).

## 10. Review follow-through (2026-10-14)

- [x] 10.1 Session continuation implemented in `main.py` (echo → live session adopts the id, fresh plan, no runs-row mutation; unknown/cross-user/soft-deleted → fresh) + `tests/test_session_continuation.py` pinning all branches (4 passed). The idle-boundary behavior was REMOVED (review M1: the SPA's resume touch made it unreachable; a returning user must be able to resume an old conversation) — spec deltas, config, and tests updated; `PYTHON_AGENT_SESSION_IDLE_MIN` does not exist.
- [x] 10.2 Review M2 — SPA `onRunId` now adopts a server-minted id whenever it differs from the id sent with the request (was: only when no id was active), so the panel never diverges from the session the server actually wrote to. Also: `startNewSession` deletes its minted session as an orphan when a send raced the create; the rail refreshes page 1 on adoption (`refreshSessionList`) so a new conversation appears immediately.
- [x] 10.3 Review M3 — `ChatStore.list_messages` ownership probe + read now run in one transaction; docstring corrected (was claiming the probe "IS the WHERE clause").
- [x] 10.4 Review L1 — `internal/chat/handler.go` decode-failure details are now static strings (no `err.Error()` on the wire), tests adjusted.
- [x] 10.5 Review L3/L5/L6 — `ChatSidebar`'s dismiss hook stays modal-semantics (fires for every row tap — the drawer e2e pins it) while the panel stays put on an active-row click; `types/chat.ts` trailing newline; `backend/agent_py/.env.example` documents `PYTHON_AGENT_SESSIONS_PAGE_SIZE`.
- [x] 10.6 E2E-caught defect: terminal persistence lost on client disconnect (SPA closes at `terminator` → Go cancels upstream → the generator's finally-block writes were cancelled mid-await; a completed answer kept only its user row and left the run `running`). Fixed with a detached, shielded finalize task in `main.py`; pinned by `test_disconnect_during_finalize_still_persists` + assistant-replay assertions in two e2e specs; spec requirement "Terminal persistence survives client disconnect" added.
- [x] 10.7 E2E-caught defect: the pre-existing `chat-rag` test `closing the dialog resets the message history` encoded the superseded ephemeral-chat contract; rewritten to the Slice-2 contract (`closing and reopening the dialog restores the active conversation`) with an assistant-replay assertion.

## 11. Archive

- [x] 11.1 Run `openspec archive agent-chat-history-ui --yes` after every other task is checked. Verify: `openspec validate --archived` passes post-archive; the change moves to `openspec/changes/archive/2026-10-14-agent-chat-history-ui/` with delta specs synced to `openspec/specs/`.

## 10. Archive

- [x] 10.1 Run `openspec archive agent-chat-history-ui --yes` after every other task is checked. Verify: `openspec validate --archived` passes post-archive; the change moves to `openspec/changes/archive/2026-10-14-agent-chat-history-ui/` with delta specs synced to `openspec/specs/`.
