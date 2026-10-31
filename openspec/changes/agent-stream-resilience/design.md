# Design

## Context

Three runtimes own one stream. The Python agent produces the frames (`/agent/run`), the Go proxy forwards them byte-for-byte (`POST /api/chat`), and the SPA parses them (`chatService.streamMessage` → `chatStore`). Recovery state already exists end to end: the SPA captures `run_id` from the first frame; Python's resume decision tree keys an echoed id on live `agent.runs` row → live session → fresh; LangGraph checkpoints carry step-boundary state; `step_trace` persists step events; the shielded-finalize requirement already guarantees terminal writes land without the client.

What is missing is the seam between them: the SPA cannot tell a cut from a clean end, the wire has no keepalive, and the run generator's lifetime is bound to the HTTP response (`asyncio.CancelledError` on client disconnect), so every disconnect today converts "transport hiccup" into "run died mid-step".

See proposal.md for motivation; behavior contract in the three spec deltas.

## Goals / Non-Goals

**Goals:**

- A cut stream is a detectable, typed state in the SPA — never a silent fake-clean end.
- Idle streams survive proxies and NAT (wire keepalive).
- A mid-run disconnect does not destroy the run; a reconnect observes the same run instead of re-executing it.
- Recovery uses the richest mechanism available and degrades gracefully: live re-attach → transcript replay → checkpoint re-POST.
- Zero change for consumers that know nothing about this feature: comment frames are spec-legal SSE, the ladder only activates on an incomplete end.

**Non-Goals:**

- Frame-level delta replay (`Last-Event-ID`, Redis Streams buffering, per-frame ids). The delta gap across a disconnect window remains; this change documents it rather than papers over it.
- Multi-instance re-attach affinity. A backgrounded run lives in one process; re-attach must reach that process. The compose deployment is single-instance for the agent; multi-instance fan-out is a Phase-3 trigger together with frame buffering.
- Read-side dead-stream detection in the Go proxy (the cap plus the SPA watchdog cover the observable cases).
- Changes to the legacy single-turn path, the sweeper, the approval wire shape, or the resume decision tree's keying.

## Decisions

**D1 — Detach lives at the Python layer, not the Go layer.**
Go keeps exactly today's behavior: the upstream request rides the client's request context, so a client disconnect closes the Go→Python connection. The run survives because Python no longer binds run execution to the response stream. *Alternative considered*: `context.WithoutCancel` in the Go proxy so Go keeps consuming upstream after the client leaves — rejected: it leaves Go holding an upstream connection whose bytes go nowhere, it duplicates lifetime policy in two runtimes, and it would not survive a Go restart anyway. One owner (Python) for run lifetime; Go remains a byte pump.

**D2 — Run execution moves into a supervisor task; the stream generator becomes a subscriber.**
The producer/consumer split already exists: `_pump_events` drives the graph into the per-run `RunSink` queue as a task, and the SSE generator drains that queue (`_drain_run_queue`). Today the pump's lifetime is still bound to the generator — the disconnect kill switch is literally `pump_task.cancel()` in the generator's `finally` (`main.py:1267`). Detach restructures ownership, not plumbing: a per-run **supervisor** task owns the pump, the heartbeat loop, the event accumulation (`assistant_text_parts` / `assistant_steps` — currently folded inside the drain loop, which dies with the client), and the shielded terminal persistence. The SSE generator becomes a pure wire encoder over the queue.
With detach on, the generator's teardown leaves the supervisor running; a client disconnect cancels the *subscription*, not the run. The supervisor wraps the pump in the wall-clock cap (`PYTHON_AGENT_DETACHED_RUN_CAP_SECONDS`) → cancel + `mark_failed` on expiry, and keeps the heartbeat until terminal so the sweeper's semantics are untouched. With detach off (opt-out), the generator's `finally` cancels the supervisor exactly where today's code cancels the pump — byte-identical to the current wire.
Unifying accumulation + terminal persistence under the supervisor for BOTH modes (rather than forking a detached-only path) is deliberate: one code path for the terminal writes, so the byte-for-byte regression tests guard the refactor itself. `run_agent_stream`'s pump kwargs (bearer token, tools, budget) are captured at POST time, so a detached run keeps using the original request's credentials — no re-auth, no stored-credential surface.
Bounded resources: a supervisor always terminates (cap, terminal trio, or exception → `mark_failed`), and the sweeper still reaps any process that dies mid-detach because the heartbeat stops with it.

**D3 — Keepalive exists; this change makes it contractual.**
Correction surfaced during design: the drain side already emits `: keep-alive\n\n` after 15 s of idle (`_drain_run_queue`, `_STREAM_KEEPALIVE_SECONDS`, landed with agent-feed-tools for the approval pause), and the byte-for-byte proxy plus the SPA's comment-skipping parser already carry it end to end. The delta is therefore small: lift the hardcoded constant into `PYTHON_AGENT_KEEPALIVE_SECONDS` (default 15, `0` disables), and build the SPA watchdog (D7) on the same interval so its absence becomes a detectable dead-stream signal instead of a blocked `reader.read()`. A comment frame stays the right shape — no event, no data, invisible to every conforming parser; a data-frame ping would alias with the `[DONE]` sentinel and require a dispatch-table entry in every consumer.

**D4 — Re-attach replays step events, not deltas.**
On re-attach the endpoint emits `note{kind:"reattached"}`, then reconstructs `plan` / `step_start` / `tool_call` / `tool_result` / `step_end` frames from the persisted step trace, then re-emits a pending `confirm_request` if the run is awaiting a decision, then bridges to the live frame queue. Deltas already streamed are *not* in the trace and are not fabricated — the partial answer on the client keeps its gap. The note kind is additive: today's parser drops a `note` carrying neither `text/provider` nor the `partial_failure` shape, so old consumers are unaffected. `run_id` semantics are unchanged: a re-attach is a *view*, not a resume — it never mutates the run row or the checkpoint.

**D5 — The ladder is ordered by information preserved.**
Incomplete end → (1) re-attach GET: cheapest path that preserves execution, and the only one that avoids re-billing the in-flight step; (2) typed 409 (run not live / unknown / cross-user — one sentinel, no existence leak) or 501 (persistence off) → transcript replay via the existing session-messages fetch: authoritative for any run that reached terminal while disconnected; (3) the transcript lacks the turn's assistant row → resume re-POST echoing `run_id`: the only path that survives a process crash or a sweeper reap, and the *default* path when detach is disabled (re-attach 501) — i.e. Phase-1 behavior is exactly the ladder's third rung. Attempts bounded (3); exhaustion surfaces a visible chat error with the partial row kept.

**D6 — Two splice policies, chosen by which rung fired.**
Re-attach: the continuation is the *same* answer, so new deltas append into the existing partial assistant row (gap documented). Resume re-POST: the in-flight step re-executes and regenerates, so the partial row's *content* is replaced by the new stream — appending would concatenate two different answers. Both policies mutate one row; neither inserts a second assistant row for the same turn.

**D7 — The watchdog closes the loop the keepalive opens.**
Keep-alive comments make liveness *visible*, so their absence is a dead-stream signal: if no frame (keep-alive included) arrives for 3× the keepalive interval, the SPA aborts the read and enters the ladder. Without this, a silently severed connection would sit on a blocked `reader.read()` until an OS-level timeout that can be hours. The watchdog arms only while streaming and is cancelled by every received frame.

**D8 — Configuration and degradation.**
`PYTHON_AGENT_KEEPALIVE_SECONDS` (default 15, `0` disables), `PYTHON_AGENT_STREAM_DETACH_ENABLED` (default `true` — the agent is this deployment's live daily path and the operator is single-user; the flag is a kill switch, not an adoption gate), `PYTHON_AGENT_DETACHED_RUN_CAP_SECONDS` (default 600, aligned with the Go proxy's `LLM_MAX_STREAM_DURATION` default of 10 m; must be ≥ 1, invalid values refuse startup — with detach on by default, the cap must have a built-in default so an unconfigured upgrade boots). Degradation matrix: persistence off → re-attach answers 501 `reattach_unavailable`, ladder rung 3 still works (resume requires no run row to be *readable* — the echoed id just mints fresh per the existing tree, matching today's bare-config behavior). Detach off (opt-out) → rungs 1–2 still function for runs that complete while the client is gone, and rung 3 is the recovery path — exactly today's manual behavior, automated. Budget: a detached run keeps consuming the user's budget after they leave — accepted and documented; the cap bounds it.

**D9 — Wire-contract tests parse with the real consumer.**
Every new frame shape (`: keep-alive` mid-stream, the re-attach replay sequence, incomplete-end) is asserted by feeding the *bytes* through the SPA's actual parser (`chatService` fixtures), per the project's producer/consumer self-consistency rule; Python-side pytest asserts the exact byte format and cadence with a fake clock. The e2e cut is induced at the browser's network layer (`context.setOffline` / route abort), not by mocking the parser.

**D10 — Cancel is an explicit call, because the connection lost its veto.**
Today Cancel works by aborting the fetch: disconnect is simultaneously "accident" and "kill signal". Detach removes the second meaning, so deliberate stop needs its own channel: `POST /agent/runs/{run_id}/cancel` → supervisor cancelled → `failed{code:"user_cancelled"}` → no assistant row for the partial answer → subscribers get the typed error frame + terminal trio. The `failed` terminal state is deliberate: the existing resume tree then treats the next echoed id as a session continuation — a cancelled run never resurrects on the next send. The SPA calls cancel *first*, then aborts locally regardless of outcome (optimistic UI; a failed cancel call degrades to today's local abort, worst case the run finishes detached into history). Presentation: the run-in-flight marker gains a stop control for backgrounded runs; `user_cancelled` renders as stopped-by-user, not an error toast. The endpoint reuses the re-attach's one-sentinel 409 (`run_not_live`) for non-supervised/unknown/cross-user — no existence leak, and detach-off callers fall back to closing the connection, which remains the kill signal in that mode.

## Risks / Trade-offs

- *[detached-run leak]* → the cap terminates every task; sweeper reaps crashed processes; exhaustion of the asyncio queue cannot block a task that no longer has subscribers.
- *[double spend]* → the resume re-POST rung still re-executes the in-flight step; it is now the *last* rung, not the only one.
- *[post-abandonment spend]* → a detached run keeps billing after the user leaves; accepted, bounded by the cap, visible in budget metrics.
- *[delta gap]* → documented limitation; the re-attach note (`reattached`) makes the gap diagnosable in the UI's run trace rather than silent.
- *[409 ambiguity]* → not-live, unknown, and cross-user share one response shape by design (no existence leak), consistent with the approvals endpoint's sentinel.
- *[proxy buffering]* → keep-alives also serve as buffer-flush probes; Go's per-chunk `Flush` and `X-Accel-Buffering: no` are already in place.

## Migration / Rollout

1. Detach ships **default-on** (`PYTHON_AGENT_STREAM_DETACH_ENABLED=true` default; the env var is the kill switch) — this deployment's agent is a live daily path with a single operator, so an opt-in gate would only delay the fix. Cap defaults to 600 s so an unconfigured upgrade boots.
2. Dev/CI completes end to end: unit, wire round-trip, `@live` Playwright cut-and-resume specs — the byte-for-byte regression suite is the guard that the supervisor refactor doesn't disturb the wire.
3. Production gates live in the sibling `agent-stream-resilience-rollout` change: green-window wait, staging disconnect drill against the default-on build, observation window for detached-cap `failed` runs and budget deltas. Rollback = set the env to `false` (ladder falls back to rung 3, which is today's manual path made automatic). No env flip task — the default is the rollout.
