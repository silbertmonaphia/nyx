# Spec Delta

## Purpose

Makes the SPA resume from the exact frame it last saw, and makes that resume gap-free rather than step-granular.

## MODIFIED Requirements

### Requirement: Recovery is an ordered ladder with bounded attempts

On an interrupted stream the SPA SHALL attempt recovery in order: (1) re-attach to the live run via `GET /api/chat/runs/{run_id}/stream` using the interrupted turn's `run_id`, presenting the last frame id it received as the `Last-Event-ID` header so the agent resumes from that exact frame; (2) on a non-200 re-attach response (409 not-live, 501 unavailable, transport error) replay the session transcript via the existing session-messages fetch; (3) if the transcript lacks the interrupted turn's assistant row, re-POST the chat request echoing the `run_id` (checkpoint resume). The ladder SHALL be bounded (default 3 recovery attempts across all rungs); exhausting it SHALL surface a visible chat error while keeping the partial assistant row. A re-attach that itself ends interrupted SHALL re-enter the ladder, subject to the same attempt bound. Recovery SHALL be cancelled by any user action that ends the turn (Cancel, a new send, navigation, session switch), and a cancelled ladder SHALL leave no in-flight requests.

#### Scenario: Live run completes via re-attach

- **WHEN** the stream is interrupted mid-run, the run is still live, and the re-attach succeeds
- **THEN** the client consumes the re-attach stream (reattach note, replayed frames, live deltas, terminal trio) and the turn completes as a normal finished turn with usage

#### Scenario: Resume from the last seen frame has no gap

- **WHEN** the SPA reconnects after having received frames up to some id, and the agent can serve that cursor
- **THEN** the missed frames arrive before the live ones, the assistant row shows the complete answer with no missing span, and no frame is rendered twice

#### Scenario: Truncated replay is surfaced, not hidden

- **WHEN** the agent answers with `note{kind:"replay_truncated"}` because the cursor is too old
- **THEN** the SPA keeps the partial answer, still completes the turn from the step projection and live frames, and marks the turn as possibly incomplete rather than silently presenting it as whole

#### Scenario: Run completed while disconnected replays the transcript

- **WHEN** the re-attach answers 409 `run_not_live` and the session transcript contains the interrupted turn's assistant row
- **THEN** the panel replays the stored transcript and the turn completes without any re-execution

#### Scenario: Crashed or reaped run falls through to resume re-POST

- **WHEN** the transcript lacks the interrupted turn's assistant row
- **THEN** the SPA re-POSTs with the echoed `run_id`, consuming the regenerated stream, and the turn completes from the resumed run

#### Scenario: Detach disabled degrades to the resume rung

- **WHEN** the agent runs with detach disabled so every re-attach answers 501 `reattach_unavailable`
- **THEN** rungs 1–2 fall through and the ladder recovers via the resume re-POST — the only behavior change from today being that it happens automatically

#### Scenario: Attempts exhaust visibly

- **WHEN** every attempt across the ladder fails or ends interrupted and the bound is reached
- **THEN** a visible chat error is shown, the partial assistant row is preserved, and no further requests fire

#### Scenario: User action cancels recovery

- **WHEN** the user presses Cancel, sends a new message, or navigates while the ladder is mid-flight
- **THEN** pending and in-flight recovery requests are aborted and no zombie request survives the action

#### Scenario: Cancel stops the run deliberately, not by dropping the connection

- **WHEN** the user presses Cancel on an active or recovering stream
- **THEN** the SPA calls the run cancel endpoint first and aborts the local stream regardless of the call's outcome; the partial assistant row is dropped exactly as today, and a failed cancel call (network down) degrades to the local abort alone — the run may finish detached into history, which is acceptable

#### Scenario: A backgrounded run is cancellable from the session view

- **WHEN** the user opens a session whose run is still executing in the background (the composer is locked by the run-in-flight marker)
- **THEN** the marker offers a stop control that calls the run cancel endpoint; success unlocks the composer and refreshes the session (no partial assistant row appears), and a failed call leaves the marker until the run's own terminal state lands

#### Scenario: Another viewer sees a deliberate stop, not a failure

- **WHEN** a stream attached in some tab receives `error{code:"user_cancelled"}` because the run was cancelled elsewhere
- **THEN** the turn is presented as stopped-by-user (an interrupted state, not an error toast)

## ADDED Requirements

### Requirement: The client tracks and deduplicates frame ids

The chat stream parser SHALL surface each frame's `id:` field with the parsed event, and the store SHALL retain the highest id received for the active run and present it on the ladder's re-attach request. The store SHALL drop any frame whose id is less than or equal to the highest id it has already rendered for that run, so a frame delivered on both the replay and the live path is rendered once. A frame arriving without an id SHALL be processed normally and SHALL NOT reset the retained cursor. The id sequence belongs to one execution, not to the conversation: a new turn starts a fresh sequence, so the store SHALL discard the retained cursor when a turn starts rather than let the previous turn's high id suppress the new turn's frames.

#### Scenario: Cursor advances as frames arrive

- **WHEN** frames with ids arrive on the primary stream
- **THEN** the store retains the highest id and presents it as `Last-Event-ID` when the ladder re-attaches

#### Scenario: Duplicate frames are dropped

- **WHEN** a frame arrives whose id is at or below the highest id already rendered
- **THEN** the frame is ignored and the rendered answer is unchanged

#### Scenario: Id-less frames do not corrupt the cursor

- **WHEN** a frame without an `id:` field arrives (replay disabled, or a degraded run)
- **THEN** it is processed normally and the retained cursor is left as it was

#### Scenario: A new turn starts a fresh id sequence

- **WHEN** the user sends another message in the same session and the agent's new execution numbers its frames from the start
- **THEN** the previously retained cursor is discarded and the new turn's frames are rendered in full
