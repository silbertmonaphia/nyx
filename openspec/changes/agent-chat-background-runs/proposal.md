# Proposal

## Why

Today any navigation in the chat panel kills the live agent run. `chatStore.startNewSession` / `setCurrentSession` / `reset` call `abortLiveStream()`, which aborts the SSE fetch — and the SSE connection **is** the run's liveness signal: Go's `streamProxy` runs the upstream call on `r.Context()`, so the disconnect tears down the request to the Python agent, whose generator gets cancelled at its next await and whose `finally` cancels the run's pump task. The user clicks `+ New chat` mid-answer and the agent simply stops — a fully-planned, half-streamed answer is lost and no assistant row is ever persisted (observed live 2026-10-14: only the user row lands in `chat_messages`). Navigation should be non-destructive: starting a new conversation, peeking at an old one, or closing the dialog must not kill an in-flight run.

## What Changes

- `frontend/src/features/chat/store/chatStore.ts` — replace abort-on-navigation with **detach**:
  - A stream is identified by a monotonic token; only the token that currently owns the panel ("attached") may write into the store. `startNewSession`, `setCurrentSession`, and `reset` (dialog close) clear the attached token instead of aborting: the fetch keeps draining (frames dropped), so the Python run completes and persists its answer server-side.
  - The `resetting` module flag is deleted — the attached-token check subsumes every case it guarded (including the cancel/abort paths, where the token stays set so the partial-row drop still happens).
  - `cancel()` (the Cancel button) and destructive session deletion (`deleteSession` of the active session) keep aborting — they are explicit stop/kill gestures.
- Detached-run surfacing:
  - New store state `runningSessionIds` + a pulse indicator on the matching `ChatSidebar` row while the detached run streams.
  - On a detached stream's terminal frame the store refreshes the rail (`refreshSessionList`) and, when the finished run's session is the active one, replays its messages — the assistant row is durable by then (agent `main.py` runs the shielded finalize **before** it yields `data: [DONE]`).
  - The composer is disabled with a short hint while the **active** session has a live detached run — a second concurrent run on the same session would interleave user/assistant rows into one conversation.
- The `run_id` frame stays the authority on the run's session id: a detached stream that has not yet adopted its id marks/refreshes against the frame id when it arrives (fresh-mint edge: echoed id unknown/soft-deleted).
- **No backend, wire, or SSE changes.** The whole change is SPA-side; the run continuing is exactly the status quo of an uninterrupted connection.

## Capabilities

### New Capabilities

(none)

### Modified Capabilities

- `chat-history-sidebar`: "Click-to-replay" and "Plus-New-chat button" no longer clear/abort the live stream — they detach it and the run continues; a new requirement pins background-run behavior (survives navigation, rail indicator, completion replay, composer guard).

## Impact

- **Code**: `frontend/src/features/chat/store/chatStore.ts` (detach mechanics), `ChatSidebar.tsx` (running indicator), `ChatPanel.tsx` (composer guard/hint), plus their vitest suites and a new `frontend/tests/e2e/chat-background-runs.spec.ts` (extends the sidebar e2e helpers).
- **Wire/backend**: none. Go proxy, Python agent, and the SSE frame contract are untouched — the change only stops the client from hanging up.
- **Docs**: `frontend/src/features/chat/README.md` if present; tick the roadmap row in `FUTURE_FRONTEND.md` (chat persistence section).
- **Dependencies**: none new.
- **Rollback**: revert the commit (no flag; no rollout gates foreseen, so no sibling rollout change — the change is completable entirely in the dev/CI environment).
