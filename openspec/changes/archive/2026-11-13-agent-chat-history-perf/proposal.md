# Proposal

## Why

Chat history feels slow and stale: clicking a session serializes a `resume` POST ahead of the replay GET and blanks the panel until both land; the replay returns the session's entire message history (full `content` + `tool_calls` + `steps` JSONB) in one unpaginated payload; and the sidebar list is fetched exactly once per mount with no refresh on tab focus, so sessions created elsewhere (another tab, another device) and updated orderings stay invisible until a full page reload.

## What Changes

- Replay fetch no longer waits for the resume POST: `setCurrentSession` fires `resume` and `messages` concurrently; a resume failure (ordering-only side effect) cannot delay or block the replay.
- `GET /api/chat/sessions/{id}/messages` (proxy path `GET /agent/sessions/{id}/messages`) supports optional pagination: `limit` + `before` (opaque message-id cursor, server-minted string) return the most recent page plus `has_more` / `next_before`. No params → full replay, byte-identical to today (backward compatible; existing clients unchanged).
- The SPA replays only the latest page (default 50) and lazy-loads older messages on scroll-up, prepending above the existing list with the existing replay-epoch guards.
- The sidebar refreshes without a reload: `visibilitychange`/`focus` triggers a single-flight, throttled page-1 refetch with the existing merge semantics (fresh rows replace in place, lazy-loaded tail preserved). No interval polling.
- Detached-run terminal refresh on the active session becomes pagination-aware: merge the fresh latest page with already-loaded older pages (dedupe by id), falling back to a page-1 reset when a gap is detected.
- While a replay fetch is in flight, `ChatPanel` renders loading skeleton rows instead of the welcome/empty state.

## Capabilities

### New Capabilities

(none)

### Modified Capabilities

- `agent-chat-sessions-api`: the per-session message replay requirement gains the optional `limit`/`before` pagination contract (wire shape, cursor semantics, 400s, backward-compatible no-param behavior).
- `chat-history-sidebar`: click-to-replay becomes latest-page + scroll-up with skeleton and non-blocking resume; a new requirement makes the list refresh on tab visibility without a reload.

## Impact

- `backend/agent_py/agent/chat_store.py` — paged `list_messages_page` (keyset on the message id, `LIMIT n+1` has_more probe).
- `backend/agent_py/agent/api/chat_sessions.py` — parse/validate `limit`/`before`, emit `has_more`/`next_before`; cursor serialized as string to keep the bigint id exact across JS.
- `backend/agent_py/migrations/versions/0009_chat_msg_session_idx.py` — `(session_id, id)` composite index so the keyset stays inside the session's slice.
- `backend/internal/chat/sessions_proxy.go` — no code change expected (GET `RawQuery` is already forwarded verbatim); add a pinning test for the query-string passthrough.
- `frontend/src/features/chat/services/chatService.ts` — new `getMessagesPage` with paging opts; response coercion for `has_more`/`next_before`.
- `frontend/src/features/chat/store/chatStore.ts` — `loadSessionInto` parallelization; `loadOlderMessages`; pagination-aware detached-run refresh; visibility-refresh wiring.
- `frontend/src/features/chat/components/ChatPanel.tsx` / `ChatSidebar.tsx` — skeleton state, scroll-up sentinel, visibility listener.
- Wire contract is additive (new optional query params + new response fields); no SSE frames, no Go route changes. One additive index migration (`0009_chat_msg_session_idx`) backs the session-scoped keyset. Rollout gates: none foreseen — everything is verifiable in dev/CI (pytest, vitest, agent-free Playwright on the side stack); no sibling rollout change is created.
