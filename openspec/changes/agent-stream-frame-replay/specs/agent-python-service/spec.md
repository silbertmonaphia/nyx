# Spec Delta

## Purpose

Wires the frame-id and buffer contract into the run stream and the re-attach endpoint, and adds the two operator knobs that bound the buffer.

## MODIFIED Requirements

### Requirement: Re-attach endpoint exposes a live run's stream

The service SHALL expose `GET /agent/runs/{run_id}/stream` under the service-token middleware with the `X-Nyx-User-Id` header required exactly like `/agent/run`. For a run that is live (non-terminal in `agent.runs`) and owned by the header user, the endpoint SHALL return HTTP 200 with the standard SSE headers and emit, in order: a `note{kind:"reattached"}` frame, then either (a) when the request carries a cursor — a `Last-Event-ID` header or a `last_event_id` query parameter — every buffered frame whose id is strictly after that cursor followed by the live frames, or (b) otherwise the run's persisted step events replayed as their normal `plan` / `step_start` / `tool_call` / `tool_result` / `step_end` frames; a re-emitted `confirm_request` frame SHALL follow when the run is paused awaiting an approval decision in case (b); the stream then continues with the run's live frames until the stream's terminal signal — the run's own `done` + `terminator` frames followed by the `[DONE]` sentinel on a normal end, or a typed `error` frame followed by `[DONE]` when the run was cancelled or capped. When the cursor names a frame the buffer has already rolled past, or frame replay is disabled, the endpoint SHALL emit `note{kind:"replay_truncated"}` before the fallback replay. The replayed stream SHALL carry the keep-alive comments. A run that is not live, unknown, or owned by another user SHALL receive one indistinguishable typed error response (HTTP 409, `code:"run_not_live"`) with no existence leak. When the persistence layer is not configured, the endpoint SHALL respond HTTP 501 with `code:"reattach_unavailable"`. The endpoint SHALL NOT mutate the run row, the checkpoint, the step trace, or the frame buffer.

#### Scenario: Live run re-attaches with replay then live frames

- **WHEN** the endpoint is called for a live owned run that has completed two tool steps and is streaming its answer, with no cursor
- **THEN** the client receives the reattach note, the two steps' frames replayed, then the answer's live `delta` frames, then `done` + `[DONE]`

#### Scenario: Cursor resume is gap-free

- **WHEN** the endpoint is called with `Last-Event-ID` naming a frame that was emitted before the disconnect
- **THEN** every buffered frame after that id is replayed in order, followed by the live frames, with no repeated id

#### Scenario: Rolled-past cursor is reported

- **WHEN** the cursor is older than the buffer's oldest retained frame or malformed
- **THEN** the endpoint emits `note{kind:"replay_truncated"}` and falls back to the step projection

#### Scenario: Pending approval is re-emitted on re-attach

- **WHEN** the run is paused awaiting an approval decision when the re-attach arrives without a cursor
- **THEN** the stream carries the step replay followed by a `confirm_request` frame with the same `approval_id` and remaining TTL, and the run stays paused until the decision lands

#### Scenario: Non-live, unknown, and cross-user runs are indistinguishable

- **WHEN** the endpoint is called respectively for a terminal run, a nonexistent run id, and another user's live run
- **THEN** all three receive the same HTTP 409 `code:"run_not_live"` envelope, and no response distinguishes which case fired

#### Scenario: Persistence disabled answers 501

- **WHEN** the endpoint is called while the service runs without the run-metadata layer
- **THEN** the client receives HTTP 501 with `code:"reattach_unavailable"` and no stream is opened

## ADDED Requirements

### Requirement: Run streams emit replayable frame ids

When `PYTHON_AGENT_FRAME_REPLAY_ENABLED=true` (the default), every frame the service emits on `/agent/run` and on a re-attach stream SHALL be appended to the run's in-memory frame buffer before it reaches any subscriber, and SHALL carry an SSE `id:` field holding the frame's zero-padded monotonic sequence. The frame's buffered bytes SHALL be the finished frame so replay is byte-identical. When the flag is `false`, the frame SHALL be emitted without an `id:` field and SHALL NOT be claimed as replayable, and the run and the stream SHALL continue unaffected.

#### Scenario: /agent/run frames carry ids

- **WHEN** frame replay is enabled and a run streams
- **THEN** every emitted frame carries an `id:` field with a strictly increasing sequence

#### Scenario: Flag off restores the pre-change wire

- **WHEN** `PYTHON_AGENT_FRAME_REPLAY_ENABLED=false`
- **THEN** no frame carries an `id:` field, nothing is buffered, and the bytes are identical to the pre-change stream

#### Scenario: Termination releases the buffer

- **WHEN** a run reaches a terminal state
- **THEN** its frame buffer is released with the session and no residual state remains in the process

### Requirement: New env keys for the frame buffer

The service SHALL accept `PYTHON_AGENT_FRAME_REPLAY_ENABLED` (boolean, default `true`) and `PYTHON_AGENT_FRAME_BUFFER_MAX` (integer ≥ 1, default 5000). A non-positive buffer size SHALL refuse startup with a message naming `PYTHON_AGENT_FRAME_BUFFER_MAX`.

#### Scenario: Defaults applied when unset

- **WHEN** neither key is set
- **THEN** replay is on, the buffer holds 5000 frames, and the service starts

#### Scenario: Invalid buffer size refuses startup

- **WHEN** `PYTHON_AGENT_FRAME_BUFFER_MAX` is zero or negative
- **THEN** startup is refused with a message naming `PYTHON_AGENT_FRAME_BUFFER_MAX`
