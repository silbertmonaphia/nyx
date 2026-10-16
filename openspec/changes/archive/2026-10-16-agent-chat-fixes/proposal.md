# Proposal

## Why

The 2026-10-16 bug review (`bugs.md`) found five user-visible defects in the agent chat surfaces, and the memory one is behavioral: preferences are stored per user and injected only into the planner prompt, but the executor — the node that actually writes the user-visible answer — never receives them, so "what do I like to eat?" comes back empty. The other four are UX: every session permanently reads "New chat" (the `title` column is never written), the history rail grows the dialog past the viewport instead of scrolling, the "Use my feeds" toggle is dead under `AGENT_ENABLED=true`, and the row timestamp is present but near-illegible (10px, relative-only).

## What Changes

- **Memory recall reaches the answer.** The rendered preferences block is threaded into the executor node as well (both `_run_llm` and `_run_react` paths), so the node that writes the reply can use stored preferences. Planner injection is unchanged; both paths preserve byte-parity when the user has no preferences (or the kill switch is off).
- **Session titles are written.** The first user message of a session sets a deterministic title immediately (whitespace-collapsed, character-truncated, CJK-safe). A one-shot best-effort refinement from the first exchange overwrites it asynchronously — failure keeps the deterministic title, the stream is never blocked or altered, and the internal call does not consume the user's chat budget. The sidebar then shows real titles; sessions with no messages keep the `New chat` placeholder.
- **Chat dialog is viewport-bounded.** The panel row is height-capped so the history rail's existing scroll container actually engages: the rail scrolls internally instead of the dialog growing with the session count (Radix keeps body scroll locked, so today's overflow is unreachable).
- **"Use my feeds" toggle removed.** Checkbox, hint, store field, localStorage key, and the dead `rag: true` field on the SPA request all go. The Go/agent wire contract is untouched: `rag` stays accepted (and ignored under `AGENT_ENABLED=true`); native clients and the legacy single-turn mode are unaffected.
- **Timestamp legibility.** Rows show an absolute local date-time alongside the relative label, at a legible size.

No breaking changes: no API, SSE, or store-schema changes.

## Capabilities

### New Capabilities

- `agent-chat-titles`: session-title generation from the first exchange — deterministic truncation plus one-shot best-effort refinement — observable through the existing sessions list API and constrained by session ownership/retention.

### Modified Capabilities

- `agent-user-prefs`: the injection requirement extends from planner-only to planner + executor (answer path); empty-set parity and the kill switch now apply to both.
- `chat-history-sidebar`: the session-row requirement updates (populated titles; legible absolute + relative timestamp) and a new requirement makes the dialog viewport-bounded with an internally scrolling rail.

## Impact

- `backend/agent_py/`: `agent/graph.py`, `agent/executor.py`, `agent/main.py`, `agent/chat_store.py`; tests `test_prefs_injection.py`, `test_executor.py`, `test_run.py`, `test_chat_store.py`, plus new title coverage. No migration expected — the `title` column exists and refinement gating uses existing row state (see design.md).
- `frontend/`: `features/chat/components/ChatPanel.tsx`, `features/chat/components/ChatSidebar.tsx`, `components/ui/Dialog.tsx` (or a panel-scoped cap), `features/chat/store/chatStore.ts`, `features/chat/services/chatService.ts`; tests `ChatPanel.test.tsx`, `ChatSidebar.test.tsx`, `chatStore.test.ts`; e2e `chat-history-sidebar.spec.ts`, `chat-rag.spec.ts` (toggle removal), `chat-user-prefs.spec.ts` (recall assertion stays plumbing-level).
- Go backend: none — the session/run proxies pass through unchanged; no OpenAPI change.
- Rollout: none foreseen — every task completes in the dev/CI environment (no prod flips, wait windows, or deployed-stack smokes). This change complements the open, PM-owned `agent-user-preferences-rollout` without altering its stated verification stance.
