# E2E evidence — agent-chat-background-runs

Run date: 2026-10-14 (evening), against the **side stack** (operator dev stack untouched):

- Go backend `:8180` (side), Python agent `:9100` (side), Vite SPA `:5273` (side, serving this worktree's edited source), host vLLM `Qwen3-4B-Instruct-2507-FP8` on `:8000`, infra from `make up` (db `:5433`, redis `:6380`, jaeger).
- Driven with `PLAYWRIGHT_BASE_URL=http://localhost:5273 npx playwright test … --workers=1` (single shared vLLM → serial; see the memory note on shared-GPU contention).

## Results

| Spec | Workers | Result |
|---|---|---|
| `tests/e2e/chat-background-runs.spec.ts` (new) | 1 | **3 passed** — final combined run after all review fixes: **9 passed** total (1.3m) |
| `tests/e2e/chat-history-sidebar.spec.ts` (regression) | 1 | **6 passed** (same combined run; helper gate tightened per review — see non-vacuity note below) |

Intermediate runs: the background-runs spec passed 1/2 → 2/2 → 3/3 as the dialog-close test, the racy sessionStorage poll fix, and the finalize-ordering rewrite landed; the combined 9-test run above is the final state.

Finalize ordering note: the detached stream's completion keeps the rail marker (composer lock) up until the active-session replay fetch resolves, then clears it — otherwise a fast user could start a new run in the just-finished session while the replay was in flight and have its panel clobbered. The 3-test spec was re-run green after that reorder.

**Boundary (explicit):** the prompts run ~15-25s at 4B speed, i.e. under the Go proxy's upstream stream deadline — historically `http.Client{Timeout: 60s}`, which severed any run past 60s mid-stream regardless of attachment (the 2026-10-14 incident). This evidence therefore covers backgrounded runs **under the deadline**; long-run (>deadline) survival is exercised by the parallel proxy change's own spec, not this one.

**Non-vacuity of the assistant-present gates (review-verified, 2026-10-14):** the `not.toHaveText(/^[\s…]*$/u)` assertions were empirically checked against a real Chromium — the locator fails on a bubble showing the `…` placeholder and fails outright when no bubble matches, so these assertions cannot pass vacuously (the same check shows the old `/^\s*$/` regex DID pass on the placeholder; the pre-existing helper in `chat-history-sidebar.spec.ts` was backported to the tighter form).

New spec coverage:

1. **New chat mid-stream** — `+ New chat` while the agent streams: the run survives (the old rail row shows the running pulse; the fresh composer is immediately usable), the pulse clears when the run finishes, and reopening the old session replays the FULL turn — user prompt + the assistant answer the backgrounded run persisted after the SPA left it (a user-row-only replay would mean the detach had killed the run).
2. **Revisiting a busy session** — clicking back into the still-streaming session locks the composer with the in-progress hint (no second concurrent run on one session); when the terminal frame lands, the hint clears and the replayed answer appears.
3. **Dialog close mid-stream** — Escape closes the dialog without aborting; reopening restores the session, shows the pulse + busy hint, and the completion replay lands.

Unit coverage (all green, `npm test` = 309 passed):

- `chatStore.test.ts` — +10 cases: detach-not-abort on `startNewSession` / `setCurrentSession` / `reset`; detached frames dropped (no index-collision leak into the newer panel); `run_id` frame re-keys the rail marker while detached and is never adopted; detached terminal refreshes the list and replays the active session; older stream's finally cannot clobber a newer stream's `isStreaming`; `send` refuses a busy active session; `deleteSession` of the active session still aborts; detached failures log instead of surfacing.
- `ChatSidebar.test.tsx` — +2: running indicator only on marked rows, with the screen-reader label; clears on unmark.
- `ChatPanel.test.tsx` — +3: composer lock + hint while the active session is busy; unlock on completion; other sessions stay unlocked.
