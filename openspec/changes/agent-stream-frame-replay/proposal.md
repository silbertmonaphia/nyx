# Proposal

## Why

`agent-stream-resilience` (archived 2026-11-01) made a run outlive its connection: a client reconnect re-attaches to the live execution, replays the run's **step events**, and continues on the live frames. What it cannot do is replay the **deltas** emitted during the disconnect window — they exist only in memory as an accumulator that feeds the final assistant row. The observable consequence is a hole in the middle of an answer:

```
before cut:  "…所以这个问题的答案"
gap:         "其实是这样的"          ← never persisted anywhere
after:       "……后续内容"
on screen:   "…所以这个问题的答案……后续内容"
```

The SSE protocol already has the answer: every frame can carry an `id:`, the client remembers the last one, and a reconnect presents `Last-Event-ID` so the server replays from that exact point. The agent emits no ids today (the SPA's parser explicitly ignores the field), so there is no anchor to resume from — the ladder must fall back to step granularity.

Closing that gap is the difference between "the answer looks mostly right" and "the answer is the answer".

## What Changes

- **Every SSE frame on the run stream carries an `id:`.** The id is the Redis Stream entry id of the frame, so it is monotonic per run and directly usable as a `Last-Event-ID` cursor. Frames are written to a per-run stream (`agent:frames:{run_id}`) **before** they reach any subscriber, so every id a client sees is already replayable.
- **A per-run frame buffer in Redis Streams.** `MAXLEN ~` + `EXPIRE` bound the memory; the retention window comfortably outlives the detach cap (`PYTHON_AGENT_DETACHED_RUN_CAP_SECONDS`, 600 s).
- **Re-attach accepts a cursor.** `GET /agent/runs/{run_id}/stream` accepts the `Last-Event-ID` header (or `last_event_id` query, for clients that cannot set it). With a cursor it replays the buffered frames strictly after that id, then switches to live frames with no gap and no duplicates — the switch point is a watermark taken at subscribe time. Without a cursor it keeps today's behavior (step projection replay).
- **Retention overflow degrades, never lies.** If the cursor names a frame older than the buffer, the endpoint answers with an explicit `note{kind:"replay_truncated"}` and falls back to the step projection, so the client knows the answer may be missing a span rather than being silently wrong.
- **Replay is a second subscriber path, not a resume.** Byte-for-byte the same frames; the run row, checkpoint and step trace are untouched, exactly as the current re-attach contract requires.
- **The SPA resumes from its cursor.** The parser captures `id:`; the ladder's re-attach rung sends the last id it saw; incoming frames are deduplicated by id so the replay/live boundary cannot show a frame twice. Two splices still apply: append on re-attach (now gap-free), replace on resume re-POST.
- **Go forwards the header.** The proxy passes `Last-Event-ID` through to the agent's re-attach route; the response stays byte-for-byte.
- **Operator switch.** `PYTHON_AGENT_FRAME_REPLAY_ENABLED` (default `true`). Off — or Redis unreachable — restores today's behavior exactly: no `id:` fields, step-projection replay, the SPA ladder unchanged.

Not changing: the run/connection decoupling itself, the resume decision tree, the cancel endpoint, the terminal trio shapes, the legacy single-turn path, `[DONE]`.

## Capabilities

### New Capabilities

- `agent-frame-replay`: the frame-id and buffer contract — id assignment, per-run Redis Stream retention, cursor semantics (replay, watermark switch, truncation), and the degradation posture.

### Modified Capabilities

- `agent-python-service`: run streams emit replayable frame ids; the re-attach endpoint accepts a cursor and replays buffered frames; two new env keys with startup validation.
- `chat-agent-proxy`: the re-attach route forwards `Last-Event-ID` (and `last_event_id`) verbatim.
- `chat-stream-resilience`: the ladder's re-attach rung carries the last-seen frame id and deduplicates by it; the interrupted partial row is completed without a gap.

## Impact

- `backend/agent_py/agent/`: `frames.py` (new — id assignment, `XADD`, `XRANGE`, watermark), `main.py` (emit path adds `id:`, re-attach cursor branch, truncation note), `config.py` (two keys).
- `backend/internal/chat/`: `runs_proxy.go` gains a request-header passthrough (no response change).
- `frontend/src/features/chat/`: `chatService.ts` parses `id:` and exposes it; `chatStore.ts` tracks the cursor, sends it on re-attach, deduplicates.
- Tests: Python buffer/cursor unit + DSN-gated Redis integration, Go header passthrough, SPA parser/cursor/dedup, a wire fixture pinning the `id:`-bearing frame bytes, and an e2e case asserting **no gap** across a cut.
- Redis becomes a hard dependency for the agent's replay path (it already backs the budget guard; health already reports `redis_unavailable`).
- Rollout gates (staging gap-free drill, green window, deploy of the default-on build, observation, rollback rehearsal) tracked in `openspec/changes/agent-stream-frame-replay-rollout/`.
