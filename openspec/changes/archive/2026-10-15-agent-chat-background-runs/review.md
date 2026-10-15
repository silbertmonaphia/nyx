# Review — agent-chat-background-runs

Fan-out review by the `test-reviewer` agent (2026-10-14, ~22:06), against `chatStore.ts` md5 `215509a7…` / `ChatPanel.tsx` `aaee261a…` / `ChatSidebar.tsx` `c1bb1a77…` and the test files of that revision. **Verdict: request changes — 0 CRITICAL, 0 HIGH, 2 MEDIUM, rest LOW/nit.** The detach design itself was verified sound: token gating consistent, no detached frame can write into the panel, no store slot can be left stuck by a user-reachable path, the no-abort assertions are real, and the collision test genuinely exercises the equal-index case.

The reviewer also empirically confirmed (real Chromium, no files written) that the e2e assistant-present gates are non-vacuous — and that the pre-existing `/^\s*$/` helper in `chat-history-sidebar.spec.ts` was NOT (it passed on the `…` placeholder).

## Required findings

| # | Sev | Finding | Disposition |
|---|---|---|---|
| V2 | MEDIUM | Spec/code drift: the delta spec said "deleting a session SHALL abort the run streaming into that session", but the code only aborts an **attached** run (a detached run has no client handle by design). | **Fixed** — spec.md narrowed: deleting aborts only the attached run; a detached run completes into the soft-deleted session (row stays out of the rail). Two scenarios added pinning both delete cases. |
| C1 | MEDIUM | Concurrent replays of one session were last-write-wins: a user-click replay and the detached-run completion replay both target the run's session, and the older (user-row-only) snapshot could resolve last and overwrite the completed turn. Same root cause as the pre-existing variant "user sends during a `setCurrentSession` fetch → replay clobbers the live turn". | **Fixed** — `loadSessionInto` now writes only if (a) no newer replay superseded it (monotonic `replaySeq`), (b) the active session still matches, and (c) no stream is attached (`attachedToken === null`). One guard closes both the new path and the pre-existing variant. |
| V1 | LOW-MED | The deliberate "hold the rail marker (composer lock) until the completion replay lands" ordering was unpinned — reverting it kept all tests green. | **Fixed** — new unit test defers `getMessages`, asserts the marker stays up (and `send` refuses) until the deferred replay resolves, then clears with the replayed rows. |
| §6 | LOW | e2e-evidence should note the assistant-present assertions are non-vacuous. | **Fixed** — recorded in `e2e-evidence.md`, with the backport below. |

## Recommended findings (all applied)

- **S1** — `handleSubmit` guarded on `isStreaming` only; an implicit submit (Enter on the checkbox) could clear the draft before the store refused it. Now guards on `composerLocked`.
- **Test isolation** — module-level stream bookkeeping (`attachedToken`/`controller`/`detachHook`/`streamSeq`) lives outside the store; a test ending mid-stream could leak a detach hook into the next test. All three test files' `resetStore` now call `clearActiveSession()` first.
- **V3** — a run detached before its `run_id` frame could never show the rail pulse (its row only appeared at the terminal refresh). The detached `onRunId` branch now also pulls page 1.
- **Two-detached-runs test** and **cancel-with-only-detached test** added.
- **Regex backport** — `chat-history-sidebar.spec.ts`'s "stream started" gate tightened to `/^[\s…]*$/u` so it cannot pass on the placeholder.

## Accepted (with rationale)

- **C2 (LOW, nit)** — `cancel()`'s half-built-row drop runs in the aborted stream's `finally`, one microtask later; a *programmatic* `send()` in the same tick could take the token first and strand the placeholder row. The reviewer confirmed it is unreachable from the UI (the composer disables synchronously and the next real click is a later task). Documented in `cancel()`'s comment rather than restructured — an eager fix would have re-introduced the old cross-stream index-slice hazard.
- **Pre-existing, out of scope (flagged by the reviewer):** the attached error path surfaces `err.message` into `store.error`/the panel while `submitApproval` deliberately does not echo it — inconsistent postures; the reviewer confirms the new paths are all quiet-logged. Left as-is; noted here for a follow-up.
- **E2E flake window (LOW):** test 1 asserts the rail pulse right after `+ New chat`; if the run finished between the gate and the click, the pulse would already be gone. Mitigated by the long prompt + serial runner; not restructured.
