# Spec Delta

## MODIFIED Requirements

### Requirement: Click-to-replay

When the user clicks a session row in the sidebar, the SPA SHALL set the active session id, fetch the session's messages via `GET /agent/sessions/{id}/messages`, and render them in `ChatPanel` in chronological order. If a token stream is live when the click lands, the stream SHALL be detached, not aborted: the SPA keeps the SSE connection open and drops its subsequent frames so the run completes server-side. The SSE stream for the next user message targets the active session id via the existing `run_id` echo.

#### Scenario: Replay renders messages in order

- **WHEN** the user clicks the 3rd session in the sidebar
- **THEN** `ChatPanel` detaches any live stream (the run continues in the background), fetches the messages, and renders them as user/assistant bubbles in chronological order; the assistant's `tool_calls` (if any) are rendered as the same "agent steps" collapsible region the live stream produces

#### Scenario: Clicking the active session is a no-op

- **WHEN** the user clicks the row of the currently-active session
- **THEN** nothing changes (no refetch, no scroll, no panel clear)

#### Scenario: Switching away mid-stream keeps the run alive

- **WHEN** the user clicks another session row while a run is streaming
- **THEN** the stream is detached rather than aborted, the clicked session's messages replay, and the detached run's session row shows the background-running indicator until the run reaches a terminal state

### Requirement: Plus-New-chat button

The SPA SHALL render a `+ New chat` button in the chat header (next to the existing chat controls). When clicked, the SPA SHALL POST `/agent/sessions` (via the Go proxy chain), receive the new session id, set it as the active session id, detach any live stream (the run continues in the background), and reset the chat panel to an empty state. Subsequent user messages are recorded under the new session. The rail SHALL NOT be refetched on create — the new session's row appears on the next list fetch (reload or the next lazy-load trigger), keeping the interaction single-round-trip.

#### Scenario: New chat clears the panel

- **WHEN** the user clicks `+ New chat`
- **THEN** the sidebar highlights no row (no active session), `ChatPanel` detaches any live stream and renders an empty welcome state; the next user message starts a fresh session

#### Scenario: New chat keeps the sidebar list

- **WHEN** the user clicks `+ New chat`
- **THEN** the existing session rows remain in the sidebar (no scroll-to-top, no list clear); only the active row changes

#### Scenario: New chat mid-stream keeps the run alive

- **WHEN** the user clicks `+ New chat` while a run is streaming
- **THEN** the run's SSE connection stays open, the run's session row shows the background-running indicator, and the run's completed answer is persisted under its original session even though the panel moved on

## ADDED Requirements

### Requirement: Background runs survive navigation

The SPA SHALL NOT abort a streaming run on navigation. Starting a new chat (`+ New chat`), switching sessions, and closing the chat dialog SHALL each detach the live stream instead: the SSE connection stays open, the SPA continues draining (and discarding) its frames, and the run completes and persists its answer server-side. The Cancel button SHALL remain the explicit stop gesture that aborts an attached run, and deleting the session a run is still attached to SHALL abort that run. A detached run has no client-side abort handle: deleting its session SHALL NOT abort it — it completes into the soft-deleted session, whose row stays out of the rail. While a detached run is live, its session row in the sidebar SHALL display a running indicator, removed when the run reaches a terminal state. When a detached run reaches a terminal state, the SPA SHALL refetch the sidebar list; when the finished run's session is the currently active one, the SPA SHALL also refetch and replay that session's messages so the completed turn appears. While the active session has a live detached run, the SPA SHALL disable the composer and show a hint that the agent is still working on that conversation (a second concurrent run on one session would interleave its messages).

#### Scenario: Closing the dialog mid-stream keeps the run alive

- **WHEN** the user closes the chat dialog while a run is streaming
- **THEN** the dialog's panel state clears but the run is not aborted; reopening the dialog restores the session and, once the run finishes, the completed turn replays

#### Scenario: Completed background run replays in full

- **WHEN** a detached run reaches a terminal state and the user opens its session
- **THEN** the session's replay renders the user message and the run's completed assistant answer

#### Scenario: Running indicator tracks the detached run

- **WHEN** a run is detached mid-stream
- **THEN** its session row renders the running indicator; the indicator disappears when the run reaches a terminal state, and the sidebar list has been refetched

#### Scenario: Composer is guarded while the active session's run is live

- **WHEN** the user opens the session of a still-streaming detached run
- **THEN** the message composer is disabled with an in-progress hint until the run reaches a terminal state, at which point the replayed turn appears and the composer re-enables

#### Scenario: Cancel still stops the attached run

- **WHEN** the user clicks Cancel while the panel's own stream is live
- **THEN** the stream aborts, the half-built assistant row is dropped, and the composer re-enables

#### Scenario: Deleting a session with an attached run aborts it

- **WHEN** the user deletes the session the panel's own stream is live on
- **THEN** the stream aborts and the panel clears

#### Scenario: Deleting a session with a detached run leaves it running

- **WHEN** the user deletes a session whose run was detached earlier
- **THEN** the run is not aborted; it completes into the soft-deleted session and the deleted row stays out of the rail
