# Design

## Context

Diagnosed 2026-11-13 from the running code (see conversation: `chatStore.ts:429-459` serialized resume→replay; `chat_store.py:324-370` unbounded full-table replay; `ChatSidebar.tsx:109-113` mount-only list fetch). Three-hop chain SPA → Go proxy → Python agent → Postgres per session call; every call shares one uvicorn event loop and asyncpg pool (max 10, acquire timeout 5s, retry backoff) with any live agent run. Indexes already exist: `chat_sessions_user_active_idx (user_id, last_active_at DESC) WHERE deleted_at IS NULL`, `chat_messages_session_created_idx (session_id, created_at)`.

## Goals / Non-Goals

Goals:
- Perceived replay latency: one click → skeleton immediately → latest page renders after a single round trip.
- Wire payload bounded: replay transfers O(page) not O(session).
- Freshness without reload: rail reflects cross-tab/cross-device changes on focus.

Non-Goals:
- Message virtualization in `ChatPanel` (render cost stays O(loaded pages); revisit if sessions reach thousands of *loaded* messages).
- gzip/Content-Encoding at the proxy (pagination bounds the payload; touching the deny-by-default response-header list is separate risk).
- `after=<id>` incremental fetch (append-only merge from fresh page 1 suffices; see Detached-run refresh).
- Any change to SSE frames, the stored schema, or the Go proxy code.

## Decisions

### D1: Cursor is a server-minted string encoding the message id (not `(created_at, id)` tuple)

`chat_messages.id` is a monotonic `IDENTITY` bigint; insertion order equals id order (messages are never updated in place), so keyset on `id` alone is stable — the `(last_active_at, id)` tuple the session list needs (its sort key mutates) is unnecessary here.

The id must cross the JS boundary exactly: `Number` loses precision above 2^53, and the frontend round-trips the cursor through `URLSearchParams` (string-typed anyway). So the API serializes `next_before` as the decimal string of the boundary id; the server decodes it back to bigint and 400s anything that isn't a decimal string of a positive integer. Clients treat it as opaque (spec'd). Per-message `id` stays a JSON number as today — only the cursor is string.

Alternative considered: base64url tuple cursor like the session list. Rejected: two cursor dialects on one endpoint family without a stability need; and tuple keyset on an immutable sort key is over-engineering.

### D2: No-params request stays "full replay" (backward compat) rather than changing the default

Existing consumers (GDPR export path reads via the store directly; the SPA is the only HTTP client and it converts in this change) — keeping no-params byte-identical means the wire contract is purely additive: an old SPA against a new agent works, a new SPA against an old agent works (response coercion treats missing `has_more` as "full"; scroll-up simply never fires). Changing the default to "latest 50" would be a smaller payload for everyone but silently breaks any un-updated client's replay completeness. Additive wins per repo convention (backward compatibility unless told otherwise).

### D3: Store gains `list_messages_page`, a separate method — `list_messages` is untouched

```sql
SELECT ... FROM agent.chat_messages
WHERE session_id=$1 AND id < $2::bigint   -- $2 omitted (NULL) for the first page
ORDER BY id DESC LIMIT $3                 -- $3 = limit + 1 (has_more probe)
```
reversed in Python before return. `id` is the PK, but ordering by `id` while filtering on `session_id` does NOT use the PK index usefully — the planner would scan the global PK backwards across other users' rows until it fills the session's page. A new composite index `chat_messages_session_id_idx (session_id, id)` (migration `0009_chat_msg_session_idx`) makes the scan start inside the session's slice and stop at `LIMIT`. The existing ownership probe + transaction wrap from `list_messages` is reused (same 404 posture). Fetching `limit + 1` proves `has_more` without a `count(*)` (same pattern as `list_sessions`).

`before` as NULL-able param keeps one SQL text for both first-page and continuation (avoids the two-SQL-text branch `list_sessions` needs because its sort key mutates; here it doesn't).

### D4: Frontend replay state: pages load downward, older pages prepend

`chatStore` gains `replayNextBefore: string | null` and `replayHasMore: boolean` alongside `messages`. `loadSessionInto` requests `{limit: 50}`; `loadOlderMessages` (new store action) single-flights via `isFetchingOlder`, threads `replayNextBefore`, and prepends with the existing `replaySeq` epoch guard (a session switch during the fetch discards it). Scroll pinning: `ChatPanel` records `scrollTop` + `scrollHeight` before the prepend lands and restores `scrollTop += (newScrollHeight - oldScrollHeight)` after — the standard compensate-on-prepend; RTL-independent because the scroll container is LTR.

Detach-run terminal refresh (the `chatStore.ts:789` path) refetches the latest page and merges: `fresh ∪ (loaded − freshIds)`, contiguous because messages are append-only and older pages sit below the fresh window. Gap case (>50 new messages while detached): fresh page 1's `has_more=true` *and* its oldest id no longer chains below the previously-loaded window → fall back to "replace everything with fresh page 1, reset `replayNextBefore`". Simplest always-correct fallback; loses scroll position only in a rare burst case.

Alternative considered: an `after_id` incremental param for terminal refresh. Rejected: adds a second read mode to the API for one caller when page-1-merge covers it.

### D5: Parallel resume + replay via `Promise.all` with isolated failure handling

`loadSessionInto` fires `resumeSession(id).catch(log)` and `getMessages(id, {limit})` concurrently; the replay write keeps its three-way guard (epoch, currentSessionId, attachedToken). The resume result was already fire-and-forget semantically (ordering hint + retention liveness) — the only real change is no longer `await`ing it before the fetch. `resumeSession` errors today already log-and-continue; nothing new to handle.

### D6: Visibility refresh = existing `refreshSessionList` + a throttled listener

`refreshSessionList` already implements the exact merge semantics (page-1 replace-in-place, tail preserved, quiet failure). The new piece is a listener in `ChatSidebar` (`visibilitychange` → `document.visibilityState === "visible"`, plus `window` `focus` for the macOS click-through case) gated by: dialog open (component mounted ⇒ true), 30s throttle (module-level `lastVisibilityRefreshAt`), single-flight via the store (a `isRefreshingList` flag or reuse of the loadMore in-flight guard). No timer, no `setInterval`. Relative timestamps update as a side effect of the re-render with fresh `last_active_at` values.

### D7: Skeleton in `ChatPanel` driven by a `isLoadingReplay` store flag

Today the panel clears `messages` and renders the welcome state during the fetch. A boolean set at the top of `setCurrentSession`/restore and cleared when the replay write lands (or on 404-clear) drives 3-4 shimmer rows. Strictly presentational; the welcome state remains the empty-session rendering.

## Risks / Trade-offs

- [Old SPA + new agent] → additive contract (D2); no-param path byte-identical, pinned by a wire test.
- [New SPA + old agent] → coercion treats absent `has_more` as full replay; scroll-up never fires; no regression.
- [Scroll pinning jank on slow devices] → compensate-on-prepend is synchronous with the store write in a layout effect; worst case a one-frame jump, not data loss.
- [Cursor forgery] → `before` is validated as a decimal bigint and owner-scoped by the same probe as the session id; a forged but valid cursor just pages that user's own messages. No security surface beyond today's.
- [Terminal-refresh fallback drops scroll position] → only when >50 messages arrived while detached; rare, and the alternative (gap-tolerant merge) is worse (silent missing messages).
- [Pool contention during a live run] → unchanged by this change (same number of queries, smaller payloads); the pagination actually *reduces* pool-held time per replay.

## Migration Plan

One additive migration: `0009_chat_msg_session_idx` creates `agent.chat_messages (session_id, id)` with `CREATE INDEX CONCURRENTLY` (autocommit block — a plain build would take a SHARE lock and block chat-message writes on a populated table; the concurrent build is a no-op when the index already exists), no data backfill. It runs on agent boot like every other migration. Deploy order is otherwise irrelevant (additive wire contract, both directions compatible): the agent service and SPA can land in either order, Go untouched. Rollback = revert either side; the index may be left in place (harmless). Verification is dev-stack: pytest (store + API + wire), vitest (store/service/component), agent-free Playwright against the side stack.

## Open Questions

(none — limit bounds [1,200], frontend page size 50, throttle 30s are pinned in the specs/design and adjustable as constants later without contract change.)
