# Design

## Context

Slice 1 (`agent-chat-history-storage`, archived 2026-10-14) gave us `agent.chat_sessions` and `agent.chat_messages` rows populated on every chat. This slice surfaces those rows: read endpoints in the Python agent service, and a sidebar-driven replay surface in the SPA. The wire contract on `/agent/run` (SSE frames, request body shape) is unchanged; the sidebar is a passive reader. See [`proposal.md`](./proposal.md) for motivation; the `agent-chat-sessions-api` and `chat-history-sidebar` specs carry the behavioral contracts.

## Goals / Non-Goals

**Goals:**
- Cursor-paginated session list (10/page, max 50) stable across same-second updates
- Per-session message replay, byte-faithful `tool_calls`
- Owner-scoped via `X-Nyx-User-Id`; cross-user requests 404 (no info leak)
- ChatGPT-style SPA sidebar with IntersectionObserver-driven lazy-load
- `+ New chat` button and per-session delete round out the surface
- `sessionStorage` persistence of the active session id (matches the existing `tokenStore` posture)

**Non-Goals:**
- Soft-delete reading (Slice 4 owns that surface — GDPR export/erase)
- Cross-session semantic recall (deferred per `FUTURE_AGENT.md` §2.1)
- Editing / re-branching a session (out of scope; not in the user pain points)
- SSE wire changes (the `/agent/run` flow is unchanged — only its `run_id` echo semantics are documented)
- Renaming, starring, or organising sessions (no UI signal in the pain points)

## Decisions

### D1. Cursor encoding: `<unix-microseconds>_<uuid>` base64url

The cursor is `(last_active_at, id)` of the last row in the current page. Encoding as `<unix-microseconds>_<uuid>` and base64url-wrapping it gives a stable, opaque, URL-safe token. Next-page query is `WHERE (last_active_at, id) < (cursor.last_active_at, cursor.id) ORDER BY last_active_at DESC, id DESC LIMIT n`. The composite-key comparison is correct even when many sessions share a `last_active_at` second — the `id DESC` tiebreaker prevents both skipped and duplicated rows.

Alternative considered: `OFFSET`-based pagination. Rejected because OFFSET scales linearly with the offset value (an N-row scan is unavoidable) and a row inserted between page fetches causes a duplicate. Cursor-based pagination is O(log N) via the existing `(user_id, last_active_at DESC)` index.

### D2. `X-Nyx-User-Id` enforced at every endpoint, not just `/agent/run`

The existing `/agent/run` route already enforces the header. The new endpoints MUST do the same — any client that talks to the agent side must carry the header (the Go chat handler always injects it). The `ChatStore` already takes `user_id` in every WHERE clause, so the data-plane defence-in-depth is in place; the endpoint just needs to read the header and pass it through.

### D3. Soft-delete on user-initiated delete, not hard-delete

The retention cron already owns hard-delete (`hard_delete_soft_deleted`). User-initiated delete just sets `deleted_at = now()`. Slice 4 (GDPR) will own the right-to-erasure path which clears every table row for a user. Until then, soft-deleted rows sit in the table until the retention cron reaps them — same posture as the existing `agent.runs` table.

### D4. SPA active session id in `sessionStorage`, not `localStorage`

`sessionStorage` is per-tab; closing the tab clears it. That matches the user's stated pain point ("关掉窗口之后就找不到之前那个对话了") — they want history across reloads but not necessarily across browser sessions. `localStorage` would survive tab close, which could be surprising (a user reopens the browser to find their last chat still active). `tokenStore` already uses `sessionStorage`; the session id lives next to it.

### D5. Sidebar pagination via IntersectionObserver, not scroll event handler

`IntersectionObserver` is the modern scroll-detection primitive — it's already polyfilled in `frontend/test/setup.js` (the existing setup means vitest + jsdom work; we get the same code path in both). A scroll listener would either need throttling or risk performance regressions; the observer's native `unobserve` API also gives us the "end of list stops fetching" behaviour for free.

### D6. Single-flight per page index in the sidebar

The lazy-load sentinel can fire twice before the first fetch resolves (the user scrolls fast). The `chatStore.loadMore` action MUST drop the second call while the first is in flight (an `isFetchingMore` boolean guards this). Same pattern as `useAuthStore.refresh` — established in the codebase, prevents duplicate-row bugs.

### D7. Clicking the active session is a no-op

A defensive equality check (`currentSessionId === clickedSessionId`) in the row's click handler avoids a refetch + panel-clear when the user re-clicks the active row. Cheap, prevents the panel "flash" that a naive re-render would cause.

### D8. Existing chat wire is unchanged — only `run_id` echo is documented

The proposal explicitly states "no new SSE frames, no new wire fields". The only delta is the existing `run_id` echo semantics being documented in the `chat-agent-proxy` spec ("Supplied session id round-trips"). This means the Go chat handler doesn't need new code paths — it already passes `run_id` through to `/agent/run`. The SPA is the one that stops minting new ids after the sidebar signals a session exists.

## Risks / Trade-offs

- **[Session list query cost]** The `(user_id, last_active_at DESC) WHERE deleted_at IS NULL` partial index keeps the list query O(log N) per user; a hot user with thousands of sessions could push the scan into the low ms range. Mitigation: cap `limit` at 50 (server-side) and run a load test in the rollout sibling before any prod flip.
- **[Cross-user leak via 404 vs 403]** A 403 would tell an attacker "this session id exists for some user"; 404 says "no such resource" and is the correct posture. Already in the spec ("Cross-user replay 404"). Implementation MUST be the `WHERE user_id=$X` filter returning no rows, not a separate "ownership check" — the latter is one bug away from leaking the existence of session ids across users.
- **[Sidebar state on token refresh]** The existing `refresh-on-401` flow re-fetches the JWT and retries the failed request. The session list endpoints aren't on the refresh path, but if the user's session id changes (rare — only on token rotation), the sidebar must refresh. Mitigation: on any 401 from the session endpoints, drop the persisted session id and render the welcome state. Already in the spec scenario "Reload after session deletion clears gracefully".
- **[N+1 on `message_count` in the list]** The list endpoint returns `message_count` per session. A naive implementation runs `SELECT count(*)` per row → N+1. Mitigation: a single `LEFT JOIN ... GROUP BY` query returns the count in one round-trip; the partial index on `chat_messages(session_id, created_at)` keeps the count cheap.
- **[Pagination cursor drift across schema changes]** If the cursor encoding changes (e.g. switch from unix-microseconds to a different timestamp source), old cursors become invalid → 400 on the next page. Acceptable: the error renders the welcome state and the user re-fetches from the top. Not a security concern.
- **[Drawer auto-close behaviour on mobile]** Clicking a session auto-closes the drawer so the user sees the loaded messages immediately. Trade-off: the user can't click two sessions in quick succession without re-opening the drawer. Acceptable — the alternative (keeping the drawer open) buries the chat panel under the overlay.

## Migration Plan

No data migration. The Slice 1 storage is already in place. The new endpoints are additive; the SPA changes are additive (the sidebar appears next to the existing chat panel — the existing live-stream behaviour is untouched for users who never open the sidebar).

Rollback: SPA flag `VITE_CHAT_HISTORY_SIDEBAR_ENABLED=false` hides the sidebar and reverts to the current behaviour (live stream only). Agent endpoints stay enabled — they're harmless if no client uses them, and removing them requires a code rollback.
