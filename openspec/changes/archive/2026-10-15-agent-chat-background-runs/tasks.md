# Tasks

## 1. Store: detach mechanics

- [x] 1.1 In `frontend/src/features/chat/store/chatStore.ts`, introduce token-gated stream ownership: a monotonic `streamSeq` + module-level `attachedToken`; every `send()` store write (event branches, the `AbortError` catch, the finally drop/`isStreaming` reset) is guarded on `attachedToken === myToken`; the module-level `resetting` flag is deleted (token check subsumes it); `controller = null` in the finally only when it still points at this stream. Add `detachLiveStream()` (clear token + controller, no abort) and keep `abortLiveStream()` (abort + clear token) for cancel/delete semantics. `startNewSession` / `setCurrentSession` / `reset` call detach. Verify: `cd frontend && npx vitest run src/features/chat/store/chatStore.test.ts`.
- [x] 1.2 Detached-terminal finalize in `send()`'s finally: when the stream ends while detached, `refreshSessionList()`, and when the run's tracked session (`run_id` frame id, updated even while detached) is the active session, re-run the existing replay (`loadSessionInto`). Verify: unit tests in 1.3.
- [x] 1.3 Update/extend `chatStore.test.ts`: navigation (`startNewSession`, `setCurrentSession`, `reset`) mid-stream leaves the mock stream **un-aborted** and continues draining; detached deltas never touch the new session's messages; a detached run's terminal refreshes the list and replays the active session; an older stream's finally cannot clear a newer stream's `isStreaming`; `cancel()` still aborts and drops the partial row; deleting the active session still aborts. Verify: `npx vitest run src/features/chat/store/chatStore.test.ts` green.

## 2. Store: background-run state + composer guard

- [x] 2.1 Add `runningSessionIds: string[]` to the store, updated only by the stream closure: eager mark on detach via a module-level hook `detachLiveStream()` pokes (covers a run sitting in a silent tool call), mark/re-key on `run_id` frames while detached (fresh-mint swap), unmark on terminal. `send()` early-returns while the active session's id is in `runningSessionIds`. Verify: unit tests in 2.2.
- [x] 2.2 Unit tests: marker appears on detach and clears on terminal; fresh-mint id re-keys the marker; `send()` refuses a follow-up on a busy active session; marker for a session that was never adopted (panel-first-message case) still lands via the `run_id` frame. Verify: `npx vitest run src/features/chat/store/chatStore.test.ts` green.

## 3. Components

- [x] 3.1 `ChatSidebar.tsx`: render a pulsing running indicator on rows whose id is in `runningSessionIds` (`data-testid="chat-session-running"`, `aria-label` tuned for screen readers). Verify: extend `ChatSidebar.test.tsx` (indicator present/absent per store state).
- [x] 3.2 `ChatPanel.tsx`: disable the textarea + Send and show an in-progress hint ("The agent is still working on this conversation…") while the active session's run is live; keep Cancel semantics unchanged. Verify: extend `ChatPanel.test.tsx` (composer disabled + hint visible while busy; re-enabled after the store clears the marker).

## 4. E2E (live stack)

- [x] 4.1 New `frontend/tests/e2e/chat-background-runs.spec.ts` (reuse the sidebar spec's helpers): send a slow prompt, click `+ New chat` mid-stream, assert the panel cleared + the old row shows the running indicator + the new composer is enabled; wait for the indicator to disappear, click the old session, assert the full turn (user prompt + non-empty assistant answer) replays; plus a case for the composer guard when reopening the still-running session. Needs `make up` + `make dev` (or the side-stack script) with a reachable LLM. Verify: the spec passes serially against the live stack.
- [x] 4.2 Run the new spec plus `chat-history-sidebar.spec.ts` (and the chat stream specs) serially — `npm run test:e2e -- tests/e2e/chat-background-runs.spec.ts tests/e2e/chat-history-sidebar.spec.ts --workers=1` — record pass/fail evidence in the change folder (`e2e-evidence.md`), verbatim on failure.

## 5. Review, docs, gates

- [x] 5.1 Spawn the `test-reviewer` agent against the diff; address CRITICAL/HIGH findings; file the report as `review.md` in the change folder.
- [x] 5.2 Docs: tick the relevant `FUTURE_FRONTEND.md` row (chat section) and update `frontend/src/features/chat/README.md` if it documents the navigation/reset behavior.
- [x] 5.3 Full frontend gates: `npm test`, `npm run lint`, `npm run build` all green; note any pre-existing failures explicitly.
