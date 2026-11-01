# Design

## Context

The archived `agent-stream-resilience` change left one documented hole: re-attach replays **step events** from the in-memory fold, so deltas emitted while the client was gone are unrecoverable. The run itself now survives the connection (supervisor task, detached registry, 600 s cap), so the missing piece is purely a transport concern — being able to name a position in the frame stream and resume from it.

Three constraints shape the design:

1. **The wire is already byte-for-byte.** Go forwards agent bytes untouched; the SPA parses them. Any id scheme must ride inside the frames, not beside them.
2. **Redis is already in the agent's critical path** (budget guard; `/healthz` reports `redis_unavailable`). A Redis-backed buffer adds no new infrastructure.
3. **Clients use raw `fetch`**, not `EventSource` — so the protocol's automatic reconnect does not exist for us, but `Last-Event-ID` as a *cursor* is still the right vocabulary and the right wire contract.

See proposal.md for motivation; behavior contract in the spec deltas.

## Goals / Non-Goals

**Goals:**

- Every frame a client receives is addressable; a reconnect from any addressable point is gap-free and duplicate-free.
- No new dependency, no new service; reuse Redis Streams.
- Strict degradation: when the buffer is unavailable or the cursor is too old, the client is told explicitly, never silently handed a hole.
- Zero change for a client that ignores `id:` (old SPA builds keep working byte-identically apart from the extra field).

**Non-Goals:**

- Cross-process replay. The buffer is shared (Redis) but the **execution** is still single-instance; a re-attach that lands on another instance can replay buffered frames but not bridge to the live run. Multi-instance stays out.
- Replay across runs (a client cannot resume a different run's stream).
- Exactly-once delivery. The contract is at-least-once with an id-based dedup on the client.
- Partial-answer persistence: a cancelled or capped run still writes no assistant row.

## Decisions

**D1 — The frame id is the Redis Stream entry id.**
`XADD agent:frames:{run_id} * ...` returns `<ms>-<seq>`, monotonic within a stream and opaque to clients — exactly the SSE `id:` contract. Reusing it removes a second counter that could drift from the buffer's own ordering, and makes the replay query trivial: `XRANGE key (cursor +`.
*Alternative considered*: a per-run integer counter stored in the frame payload. Rejected — it duplicates ordering information Redis already owns, and a restart would need to recover it from the buffer anyway.

**D2 — Write before fan-out, and fan out the id with the frame.**
The single emit path becomes: encode the frame → `XADD` → broadcast `(entry_id, bytes)` to subscribers. A client therefore never holds an id that is not yet in the buffer, which is what makes "resume from my last id" sound. The cost is one Redis round-trip per frame; deltas are the high-frequency case, so the write is pipelined/`await`ed per frame but batched only if measurements demand it.
*Alternative considered*: write async, broadcast immediately. Rejected — a disconnect in the gap between broadcast and write would hand the client an unreplayable id, which is precisely the failure this change exists to prevent.

**D3 — Buffered bytes are the finished SSE frames, not event objects.**
`event: delta\ndata: {...}\n\n` is stored verbatim. Replay is then a byte copy — no re-serialization, no chance of the replayed frame differing from the original (the same reasoning that makes the Go proxy byte-for-byte). The id line is part of the stored bytes, so a replayed frame is indistinguishable from the original.

**D4 — The replay/live switch is a watermark, not a race.**
On subscribe, the re-attach handler reads the buffer's current tail id (`XREVRANGE … COUNT 1`) as the watermark, replays `XRANGE (cursor watermark]`, then bridges to a subscriber queue that only carries frames with id > watermark. Because the emit path is single and ordered (D2), every live frame is strictly newer than the watermark, so no frame is delivered twice and none is skipped. This replaces the current "hope the queue and the replay don't overlap" posture.
*Alternative considered*: deduplicate on the client only. Rejected as the primary mechanism — it works, but a server-side watermark makes the boundary a property of the contract instead of a client assumption (the client dedup stays as a belt to these suspenders).

**D5 — Retention is bounded twice, and truncation is loud.**
`MAXLEN ~ FRAME_BUFFER_MAX` (default 5000) and `EXPIRE FRAME_BUFFER_TTL_SECONDS` (default 3600 — six times the detach cap). If the cursor is below the buffer's oldest id (`XRANGE key - + COUNT 1`), the endpoint emits `note{kind:"replay_truncated"}` and falls back to the step projection. The client keeps its partial row and may still be missing a span — but it knows, and the UI can say so.
*Alternative considered*: hold the connection open until a retry could replay. Rejected — unbounded wait for a condition that may never clear.

**D6 — The cursor travels as the protocol says; a query param is the fallback.**
`Last-Event-ID` header when present (the protocol's own channel; Go forwards it), else `last_event_id` query param (curl, native clients, and any proxy that strips unknown headers). Absent → today's step-projection replay, unchanged.

**D7 — Degradation is a configuration switch, not a code path fork.**
`PYTHON_AGENT_FRAME_REPLAY_ENABLED=false` (or a Redis failure at emit time) means: no `id:` on frames, no `XADD`, step-projection replay, the existing SPA ladder unchanged. The emit path checks one flag; there is no duplicated streaming loop. A Redis write failure mid-run degrades that run to the no-id wire for its remaining frames — the client simply re-attaches the old way, without a gap guarantee.

**D8 — The client deduplicates monotonically.**
The parser surfaces `id:` with each event; the store keeps the highest seen id per run and drops any frame whose id is `<=` it. Redis Stream entry ids sort lexicographically in the same order they were appended (equal-width ms + seq), so the comparison is a plain string comparison. This makes the client robust to any future server-side overlap, independent of D4.

**D9 — Wire-contract tests pin the id-bearing bytes.**
The existing fixture pair (Python emitter ↔ TypeScript parser) gains an `id:`-bearing fixture, generated by the real emitter, and the SPA test asserts the parser surfaces the id and the dedup uses it. A producer-side rename of the field or a change in id shape fails the Python half.

## Risks / Trade-offs

- *[per-frame Redis round-trip]* → deltas arrive at tens per second at most for a 4B model; a local Redis is sub-millisecond. If profiling shows contention, batch with pipelining without changing the contract (the id is still assigned by Redis).
- *[buffer memory]* → bounded by `MAXLEN ~` and TTL; worst case a few MB per live run.
- *[replay of a very long answer]* → `XRANGE` is capped by the watermark; the client receives what it missed, which is bounded by the disconnect window, not the whole answer.
- *[Redis loss mid-run]* → frames after the loss carry no id and are not buffered; a client reconnecting after that point falls back to the step projection for those frames. Documented; the alternative (buffering in memory as a fallback) would make the id space non-monotonic across a Redis restart.
- *[old SPA builds]* → they ignore `id:`; behavior is exactly today's. The feature is forward-compatible by construction.

## Migration / Rollout

1. Ships default-on (`PYTHON_AGENT_FRAME_REPLAY_ENABLED=true`); the flag is the kill switch.
2. Dev/CI: unit tests for id assignment/cursor/truncation, a wire fixture, Go header passthrough, SPA parser + dedup, and an e2e that cuts a stream and asserts the answer has no gap.
3. Production gates in `agent-stream-frame-replay-rollout`: staging gap-free drill, green window, deploy, observation of Redis memory/latency, rollback rehearsal.
