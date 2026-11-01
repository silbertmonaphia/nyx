# Spec Delta

## Purpose

Gives every frame on a run stream an addressable identity and a bounded, replayable buffer, so a reconnecting client can resume from the exact frame it last saw — the transport-level complement to the run-level detachment delivered by `agent-stream-resilience`.

## ADDED Requirements

### Requirement: Every run-stream frame carries a replayable id

When frame replay is enabled, the service SHALL write every frame it emits on a run stream (the original stream and any re-attach stream) to a per-run Redis Stream before the frame reaches any subscriber, and SHALL emit that frame with an SSE `id:` field whose value is the Redis Stream entry id of the write. The stored value SHALL be the finished frame bytes (`event:` line, `data:` line, terminating blank line) so a replayed frame is byte-identical to the original.

#### Scenario: Frames carry the buffer's entry id

- **WHEN** a run emits a `delta` frame with frame replay enabled
- **THEN** the frame is written to `agent:frames:{run_id}` and reaches the client with `id: <ms>-<seq>` matching that entry, ahead of the `event:` and `data:` lines

#### Scenario: A client never sees an id that is not buffered

- **WHEN** a frame is emitted
- **THEN** the Redis write completes before the frame is broadcast to any subscriber, so every id a client holds can be replayed

#### Scenario: Replay is byte-identical

- **WHEN** a buffered frame is replayed on a re-attach stream
- **THEN** the bytes delivered (including the `id:`, `event:` and `data:` lines and the terminating blank line) are identical to the bytes originally emitted

#### Scenario: Replay disabled

- **WHEN** `PYTHON_AGENT_FRAME_REPLAY_ENABLED=false`
- **THEN** no frames are written to Redis, no `id:` field is emitted, and the wire is identical to the pre-change stream

### Requirement: The buffer is bounded in size and time

The per-run buffer SHALL be trimmed to at most `PYTHON_AGENT_FRAME_BUFFER_MAX` entries (approximate trimming is acceptable) and SHALL expire after `PYTHON_AGENT_FRAME_BUFFER_TTL_SECONDS`, which MUST exceed the detached-run cap so a client can always replay for at least as long as a run can be alive. The stream SHALL be removed on the run's terminal state in addition to the expiry, so a completed run leaves no buffer behind beyond its retention.

#### Scenario: Buffer is trimmed

- **WHEN** a run emits more frames than `PYTHON_AGENT_FRAME_BUFFER_MAX`
- **THEN** the oldest entries are dropped and the newest remain replayable

#### Scenario: Buffer expires

- **WHEN** `PYTHON_AGENT_FRAME_BUFFER_TTL_SECONDS` elapses after the last write
- **THEN** the stream key no longer exists and a cursor into it is treated as truncated

#### Scenario: Terminal run leaves no buffer beyond retention

- **WHEN** a run reaches a terminal state
- **THEN** its buffer is deleted; a cursor presented afterward reports truncation rather than replaying into a finished run

### Requirement: A cursor resumes the stream without gaps or duplicates

A re-attach request carrying a `Last-Event-ID` header (or a `last_event_id` query parameter when the header is absent) SHALL replay every buffered frame whose id is strictly after the cursor, then switch to live frames at a watermark taken when the subscription is established. No frame SHALL be delivered twice and no frame emitted between the cursor and the watermark SHALL be skipped. The switch SHALL be server-side: the guarantee does not depend on the client deduplicating.

#### Scenario: Gap-free resume

- **WHEN** a client reconnects with the last id it received and frames were emitted in the meantime
- **THEN** it receives every missed frame in order, followed by the live frames, with no missing and no repeated id

#### Scenario: Resume with no missed frames

- **WHEN** a client reconnects with the id of the most recent frame
- **THEN** it receives the live frames only

#### Scenario: Unknown cursor is treated as truncation

- **WHEN** the cursor is malformed or names an id older than the buffer's oldest entry
- **THEN** the endpoint emits `note{kind:"replay_truncated"}` and falls back to the step projection, and does not replay a partial or out-of-order span

#### Scenario: No cursor keeps today's behavior

- **WHEN** a re-attach request carries neither the header nor the query parameter
- **THEN** the endpoint replays the step projection exactly as before this change

### Requirement: Replay degrades to the existing wire on failure

A Redis write failure SHALL NOT fail the run or the stream: the affected frame (and subsequent frames while the failure persists) SHALL be emitted without an `id:` field and SHALL NOT be buffered, and the client SHALL fall back to the step-projection replay on its next reconnect. A frame emitted without an id SHALL never be claimed as replayable.

#### Scenario: Redis write failure mid-run

- **WHEN** the buffer write fails while a run is streaming
- **THEN** the run continues, the frames are emitted without ids, and no error frame is synthesized for the failure

#### Scenario: Client reconnects after an unbuffered stretch

- **WHEN** a client presents a cursor from before an unbuffered stretch
- **THEN** the endpoint reports truncation and falls back to the step projection rather than replaying a span that skips frames
