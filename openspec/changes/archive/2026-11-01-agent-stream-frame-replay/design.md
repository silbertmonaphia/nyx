# Design

## Context

The archived `agent-stream-resilience` change left one documented hole: re-attach replays **step events** from the in-memory fold, so deltas emitted while the client was gone are unrecoverable. The run itself now survives the connection (supervisor task, detached registry, 600 s cap), so the missing piece is purely a transport concern — being able to name a position in the frame stream and resume from it.

Two constraints shape the design:

1. **The wire is already byte-for-byte.** Go forwards agent bytes untouched; the SPA parses them. Any id scheme must ride inside the frames, not beside them.
2. **Redis is not a durability layer in this project.** The Go cache is best-effort and opt-in; the agent uses Redis for the budget guard (degrades to `unavailable`) and the approval broker (fails closed). Nothing is persisted there, and nothing should be — a frame log in Redis would add a failure mode that does not exist today: *the run is alive but its replay buffer vanished*.

The decisive observation: **re-attach already requires the same process.** The detached registry that decides "is this run live here?" is `app.state.detached_runs`, an in-memory dict. A cross-instance shared buffer would buy nothing, because an instance that does not hold the run cannot bridge to its live frames anyway. The buffer's lifetime is therefore naturally the run's lifetime — and the run lives in exactly one process.

See proposal.md for motivation; behavior contract in the spec deltas.

## Goals / Non-Goals

**Goals:**

- Every frame a client receives is addressable; a reconnect from any addressable point is gap-free and duplicate-free.
- The buffer dies with the run: no residue, no TTL to tune, no storage dependency.
- Strict degradation: when the buffer has rolled past a cursor, the client is told explicitly, never silently handed a hole.
- Zero change for a client that ignores `id:` (old SPA builds keep working byte-identically apart from the extra field).

**Non-Goals:**

- Cross-process replay. The execution is single-instance; multi-instance remains the same non-goal it was in `agent-stream-resilience`.
- Replay across runs (a client cannot resume a different run's stream).
- Exactly-once delivery. The contract is at-least-once with an id-based dedup on the client.
- Surviving a process restart. A restart kills the run too; that path is checkpoint resume, unchanged.
- Partial-answer persistence: a cancelled or capped run still writes no assistant row.

## Decisions

**D1 — The buffer is an in-memory ring on the run's own session object.**
`_RunSession` gains `frame_log: collections.deque(maxlen=FRAME_BUFFER_MAX)` holding `(seq, frame_bytes)`, plus a monotonic `frame_seq`. Every frame the session emits is appended before fan-out. The buffer's lifetime is the session's lifetime: when the run finalizes and the registry entry is dropped, the deque goes with it.
*Alternative considered*: Redis Streams (`XADD`/`XRANGE`). Rejected on two counts. First, it makes a best-effort cache a durability dependency — Redis loss would produce "run alive, buffer gone", a failure mode that does not exist today, and the project's stated posture is that Redis never holds the only copy of anything that matters. Second, the feature's purpose (bridge a reconnect to a **live** run) already requires landing on the instance that holds the run, so a shared buffer buys nothing. If multi-instance execution ever becomes real, the buffer moves then — behind the same cursor contract, so the wire does not change.

**D2 — Append before fan-out, and carry the seq with the frame.**
The single emit path becomes: encode the frame → assign `seq` → append `(seq, bytes)` to the ring → broadcast `(seq, bytes)`. A client therefore never holds an id that is not yet in the buffer, which is what makes "resume from my last id" sound. An in-memory append cannot fail short of process death, so this invariant has no degradation branch.
*Alternative considered*: assign the id at broadcast time. Rejected — a disconnect between broadcast and append would hand the client an unreplayable id, precisely the failure this change exists to prevent.

**D3 — Buffered bytes are the finished SSE frames, not event objects.**
`id: <seq>\nevent: delta\ndata: {...}\n\n` is appended verbatim. Replay is then a byte copy — no re-serialization, no chance of the replayed frame differing from the original (the same reasoning that makes the Go proxy byte-for-byte).

**D4 — The replay/live switch is a watermark, not a race.**
On subscribe, the re-attach handler reads the session's current `frame_seq` as the watermark and replays every buffered frame with `seq > cursor`, then bridges to a subscriber queue that only carries frames with `seq > watermark`. Because the emit path is single and ordered (D2), every live frame is strictly newer than the watermark, so no frame is delivered twice and none is skipped. This replaces the current "hope the queue and the replay don't overlap" posture.

**D5 — Retention is bounded by size, and truncation is loud.**
`deque(maxlen=FRAME_BUFFER_MAX)` (default 5000) drops the oldest automatically. If the cursor names a frame whose successor the ring has already dropped (or the cursor is malformed), the endpoint emits `note{kind:"replay_truncated"}` and falls back to the step projection. The client keeps its partial row and may still be missing a span — but it knows, and the UI can say so.
No TTL: the buffer's lifetime is the run's, and a run is bounded by the detach cap. A finished run's session is released, so nothing outlives it.

**D6 — The cursor travels as the protocol says; a query param is the fallback.**
`Last-Event-ID` header when present (the protocol's own channel; Go forwards it), else `last_event_id` query param (curl, native clients, and any proxy that strips unknown headers). Absent → today's step-projection replay, unchanged.

**D7 — The id is a plain decimal integer, compared numerically.**
`id: 42` on the wire (SSE ids are text; both ends parse it as an int). The client's "already rendered" check is a numeric comparison — parsing one integer per frame is negligible next to the frame's own JSON parse. No zero-padding: it would buy a string-only comparison the client doesn't need, at the cost of readability and frame width, against the industry norm (EventStoreDB and Kafka offsets are plain integers).

**D8 — Degradation is one flag, not a code-path fork.**
`PYTHON_AGENT_FRAME_REPLAY_ENABLED=false` means: no ids, no ring appends, step-projection replay, the existing SPA ladder unchanged. The emit path checks one flag; there is no duplicated streaming loop. There is no write-failure branch to defend against, because the write is a deque append.

**D9 — The client deduplicates monotonically.**
The parser surfaces `id:` with each event; the store keeps the highest seen id per run and drops any frame whose id is `<=` it. This makes the client robust to any future server-side overlap, independent of D4.

**D10 — Wire-contract tests pin the id-bearing bytes.**
The existing fixture pair (Python emitter ↔ TypeScript parser) gains an `id:`-bearing fixture, generated by the real emitter, and the SPA test asserts the parser surfaces the id and the dedup uses it. A producer-side change to the id shape fails the Python half.

## Risks / Trade-offs

- *[numeric ids are run-scoped counters]* → cursors only make sense within one run; a cursor from run A is numerically valid-looking against run B. Mitigated twice: the store keys the retained cursor by run id, and the server's resume tree resolves run ownership before any replay, so a foreign cursor lands in the truncation or 409 branch, never in a silent mis-replay.
- *[plain counters disclose volume]* → an observer of the stream can count frames and estimate generation speed. Irrelevant for a self-hosted deployment; if this ever matters, the cursor becomes an opaque token behind the same contract (ordering then lives server-side).
- *[wire format drift]* → the spec pins "plain decimal sequence, compared numerically"; a future reformat (prefixes, padding) would silently break old clients' comparisons, so it requires a spec change, not a drive-by.
- *[memory per run]* → 5000 frames × a few hundred bytes ≈ low single-digit MB per active run, bounded by the per-user stream rate limit and the budget guard. A failed allocation surfaces as an ordinary Python error on that run, not a new global failure mode.
- *[buffer lost on process restart]* → the run is lost with it; recovery is checkpoint resume, exactly as before this change. No regression.
- *[replay of a very long answer]* → capped by the watermark; the client receives what it missed, bounded by the disconnect window, not the whole answer.
- *[old SPA builds]* → they ignore `id:`; behavior is exactly today's. Forward-compatible by construction.

## Known limits

- **The replay window is "while the run is live".** The buffer dies with the run (D5), and a re-attach can only reach a live one — a client that comes back after the run finished gets the 409 and falls to the ladder's transcript rung. That is the intended shape, but it bounds what the frame ids buy today: the agent flushes its frames at the LangGraph node boundary (the planner's chunk, then the executor's chunk) and at the approval gate's mid-node flush, so the frames a client can miss *while the run is still executing* are the planner's and the gate's — not a token-by-token answer stream.
- **The payoff scales with per-token streaming.** The executor's llm step is one `ainvoke` → one `delta` frame at the node boundary. Once the executor streams (a per-chunk delta), the "client missed the middle of the answer" case becomes the common case rather than the rare one, and the frame replay is what closes it. Until then the ladder's step-projection rung covers most real interruptions.
- **The cursor is per execution, not per conversation.** A session's later turn starts a fresh sequence, so the SPA discards the retained cursor when a turn starts (`chat-stream-resilience` spec) — otherwise the previous turn's high id would suppress the new turn's frames.

## Migration / Rollout

1. Ships default-on (`PYTHON_AGENT_FRAME_REPLAY_ENABLED=true`); the flag is the kill switch.
2. Dev/CI: unit tests for seq assignment/cursor/truncation, a wire fixture, Go header passthrough, SPA parser + dedup, and an e2e that cuts a stream and asserts the answer has no gap.
3. Production gates in `agent-stream-frame-replay-rollout`: staging gap-free drill, green window, deploy, observation of per-run memory (no new infrastructure to watch), rollback rehearsal.
