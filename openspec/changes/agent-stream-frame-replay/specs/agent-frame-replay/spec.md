# Spec Delta

## Purpose

Gives every frame on a run stream an addressable identity and a bounded, replayable buffer that lives and dies with the run, so a reconnecting client can resume from the exact frame it last saw — the transport-level complement to the run-level detachment delivered by `agent-stream-resilience`.

## ADDED Requirements

### Requirement: Every run-stream frame carries a replayable id

When frame replay is enabled, the service SHALL append every frame it emits on a run stream (the original stream and any re-attach stream) to the run's in-memory frame buffer before the frame reaches any subscriber, and SHALL emit that frame with an SSE `id:` field whose value is a plain decimal sequence, strictly increasing across the run, compared numerically by consumers. The buffered bytes SHALL be the finished frame (`id:` line, `event:` line, `data:` line, terminating blank line) so a replayed frame is byte-identical to the original. The buffer SHALL be owned by the run's session and its lifetime SHALL be the run's lifetime: it SHALL be released when the run reaches a terminal state and SHALL never outlive the process that holds the run.

#### Scenario: Frames carry a monotonic id

- **WHEN** a run emits frames with frame replay enabled
- **THEN** each frame carries `id: <decimal sequence>`, strictly increasing across the run's frames

#### Scenario: A client never sees an id that is not buffered

- **WHEN** a frame is emitted
- **THEN** the buffer append completes before the frame is broadcast to any subscriber, so every id a client holds can be replayed

#### Scenario: Replay is byte-identical

- **WHEN** a buffered frame is replayed on a re-attach stream
- **THEN** the bytes delivered are identical to the bytes originally emitted

#### Scenario: Replay disabled

- **WHEN** `PYTHON_AGENT_FRAME_REPLAY_ENABLED=false`
- **THEN** no frames are buffered, no `id:` field is emitted, and the wire is identical to the pre-change stream

### Requirement: The buffer is bounded and dies with the run

The buffer SHALL retain at most `PYTHON_AGENT_FRAME_BUFFER_MAX` frames (default 5000), dropping the oldest first, and SHALL exist only in the process that executes the run. There SHALL be no external storage, no TTL, and no residue after the run reaches a terminal state: a cursor presented after the run ended SHALL be reported as truncated rather than replayed, because a re-attach never targets a terminal run.

#### Scenario: Buffer is trimmed

- **WHEN** a run emits more frames than `PYTHON_AGENT_FRAME_BUFFER_MAX`
- **THEN** the oldest frames are dropped and the newest remain replayable

#### Scenario: Buffer is released with the run

- **WHEN** a run reaches a terminal state
- **THEN** its buffer is released with the session, and a cursor into the finished run reports truncation

#### Scenario: No external storage is consulted

- **WHEN** the buffer is written or read
- **THEN** no network call is made; the data lives in process memory only

### Requirement: A cursor resumes the stream without gaps or duplicates

A re-attach request carrying a `Last-Event-ID` header (or a `last_event_id` query parameter when the header is absent) SHALL replay every buffered frame whose sequence is strictly after the cursor, then switch to live frames at a watermark taken when the subscription is established. No frame SHALL be delivered twice and no frame emitted between the cursor and the watermark SHALL be skipped. The switch SHALL be server-side: the guarantee does not depend on the client deduplicating.

#### Scenario: Gap-free resume

- **WHEN** a client reconnects with the last id it received and frames were emitted in the meantime
- **THEN** it receives every missed frame in order, followed by the live frames, with no missing and no repeated id

#### Scenario: Resume with no missed frames

- **WHEN** a client reconnects with the id of the most recent frame
- **THEN** it receives the live frames only

#### Scenario: Rolled-past cursor is treated as truncation

- **WHEN** the cursor is malformed or names a frame older than the buffer's oldest retained frame
- **THEN** the endpoint emits `note{kind:"replay_truncated"}` and falls back to the step projection, and does not replay a partial or out-of-order span

#### Scenario: No cursor keeps today's behavior

- **WHEN** a re-attach request carries neither the header nor the query parameter
- **THEN** the endpoint replays the step projection exactly as before this change

### Requirement: Disabling replay touches nothing else

Turning frame replay off SHALL NOT affect run execution, the detached-run lifecycle, the cancel endpoint, or any other frame's shape. A frame emitted without an id SHALL never be claimed as replayable, and a client that presents a cursor to a replay-disabled service SHALL receive the truncation note and the step projection.

#### Scenario: Flag off leaves the run healthy

- **WHEN** `PYTHON_AGENT_FRAME_REPLAY_ENABLED=false` while runs are streaming
- **THEN** runs complete normally, frames carry no ids, and nothing is buffered

#### Scenario: Cursor against a replay-disabled service

- **WHEN** a client presents `Last-Event-ID` and replay is disabled
- **THEN** the endpoint reports truncation and falls back to the step projection rather than replaying or erroring
