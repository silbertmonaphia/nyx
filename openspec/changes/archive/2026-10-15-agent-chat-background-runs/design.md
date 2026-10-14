# Design

## Context

See proposal.md — Why. The constraints that shape the approach:

- **The SSE connection is the run's liveness signal.** `chatService.streamMessage` uses raw `fetch` with an `AbortSignal`; Go's `streamProxy` runs `AgentProxy.Run` on `r.Context()`, so a client disconnect cancels the upstream request, and Python's run generator is cancelled at its next await (its `finally` cancels the pump task). There is no server-side run registry a reconnect could attach to — the client cannot "come back" to a live run.
- **Terminal persistence is durable before the client sees the end.** `main.py`'s shielded finalize (assistant row + `end_session`) is awaited *before* `data: [DONE]` is yielded, so a client that has received the terminator can immediately refetch and see the completed turn. No post-completion retry loop is needed.
- **`run_id` frame authority.** The first frame carries the server's actual session id; it can differ from the echoed id when the latter is unknown/soft-deleted/another user's (fresh mint). A detached stream may still be waiting for this frame when navigation happens.
- **One composer, one panel.** The store renders exactly one message list and has one module-level controller slot; today at most one stream exists per tab.

## Goals / Non-Goals

**Goals:**

- Navigation (`+ New chat`, session switch, dialog close) never kills a run.
- A detached run stays truthful in the UI: rail indicator while live, full replay after completion.
- Keep the change SPA-only: zero backend/wire/SSE changes.

**Non-Goals:**

- Re-attaching a live stream when the user returns to a running session mid-run (the SSE connection belongs to the detached consumer; the panel replay of a still-running session shows only persisted rows until the run completes). Follow-up candidate.
- Multiple concurrent runs **on one session** — explicitly guarded against (composer disabled while the active session's run is live).
- Surviving a page reload with live runs (a reload kills the fetch; unchanged from today).
- Server-side cancel/stop for detached runs — a detached run has no client handle; it runs to its terminal state.

## Decisions

**D1 — Detach instead of abort; token-gated store writes.** A module-level monotonic token identifies the stream that owns the panel ("attached"); every store write in `send()` is gated on `attachedToken === myToken`. Navigation clears the token (frames drain and are dropped), rather than aborting. The existing `resetting` boolean is deleted — the token check subsumes all its uses, including the cancel path (where the token stays set, so the partial-row drop still runs) and the aborted-by-reset path (token cleared, drop skipped). *Alternative considered:* a per-session message map with concurrent attached streams — rejected: it is the "re-attach" non-goal's machinery, and the sidebar renders one panel, so only one stream can ever be attached.

**D2 — Terminal handling for detached runs.** On the detached stream's finally: unmark the rail (`runningSessionIds`), `refreshSessionList()`, and — when the run's session is the active one — re-run the existing `loadSessionInto` replay. Keying off the terminal frame (not `done`) is safe because the persisted row precedes `[DONE]`. The run's session id is tracked from the `run_id` frame even while detached (never adopted into `currentSessionId` when detached — the panel already moved on), so a fresh-mint id still lands the right marker/refresh.

**D3 — Rail indicator as explicit store state.** `runningSessionIds: string[]` is updated by the stream itself: eagerly when it is detached (via a module-level hook the detach helper pokes — waiting for the next SSE frame could lag a run sitting in a long silent tool call; heartbeat comment frames are dropped by the parser), and on later `run_id` frames while detached. Removed on terminal. *Alternative considered:* deriving the marker from the presence of a module-level "detached controllers" map — rejected: not reactive, and the component would need to subscribe to module state anyway.

**D4 — Composer guard on the active session.** `send()` early-returns when the active session has a live detached run, and `ChatPanel` disables the textarea + Send with an in-progress hint. Without this, the plausible flow "peek back at the running session, type a follow-up" starts a second run on the same session; both runs append to one `chat_messages` stream and the replayed pairing garbles. *Alternative considered:* allow it and let the server interleave — rejected: it corrupts the conversation's replay.

**D5 — Cancel and delete keep aborting.** Cancel is the explicit stop gesture (its existing UI contract: abort + drop the half-built assistant row). Deleting the active session aborts the run streaming into it — a destructive gesture supersedes background survival. Everything else is non-destructive navigation.

## Risks / Trade-offs

- [A stale detached run's deltas were dropped, so a mid-run peek shows only the user row] → Accepted: the composer guard explains the state; the completion hook replays the full turn. Re-attach is the named follow-up.
- [A detached run that fails persists no assistant row (server records only on `done`), so the failure is silent in the panel] → Accepted for this slice: the run's session replays as user-only; surfacing background failures in the rail is a follow-up candidate.
- [Browser connection cap: detached runs hold open fetches (HTTP/1.1 ≈ 6/origin in dev)] → Bounded in practice by the user's navigation cadence and each run's completion; prod serves the SPA over HTTP/2 via nginx.
- [The composer guard keys off `runningSessionIds`, which depends on the `run_id` frame for brand-new sessions (empty marker window between detach and first frame)] → The marker/guard only matter when the user looks at *that* session; the frame arrives as the stream's first frame, and the eager detach hook covers every detach after it.
- [Logout mid-run: a detached fetch keeps its old bearer until the run ends] → Unchanged from today's in-flight behavior (an attached stream had the same property); the completion refresh 401s quietly. Not widened by this change.
- [Detached runs are bounded by the backend's upstream stream deadline] → The SPA cannot extend the server side: the Go agent proxy bounds each upstream call — historically `http.Client{Timeout: 60s}`, which severed ANY run past 60s mid-stream (no `[DONE]`, no assistant row) regardless of attachment; a fix raising that bound lands separately in the proxy. The e2e here verifies backgrounding for runs under the deadline; long-run survival is that proxy change's gate. Frontend behavior under a severed run is consistent: the stream simply ends without `[DONE]`, the detached finalize treats any stream end as terminal, refreshes the rail, and replays what was persisted (user row only) — no hang, no stuck marker.

## Migration Plan

No data or wire migration. Deploy = ship the SPA build. Rollback = revert the commit (no flag; behavior fully covered by unit + e2e tests). No rollout gates.

## Open Questions

(none)
