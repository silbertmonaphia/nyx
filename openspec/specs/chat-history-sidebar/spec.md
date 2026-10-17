# chat-history-sidebar Specification

## Purpose
Lets users see and replay their past chat sessions via a ChatGPT-style sidebar in the SPA. The sidebar lists sessions in reverse chronological order with lazy-load, click-to-load replay into `ChatPanel`, and a `+ New chat` button. The wire contract (`/api/chat` request body `run_id`, SSE frames) is unchanged; the sidebar is a passive reader of the Slice 1 storage via the agent-side REST endpoints.

## Requirements

### Requirement: Sidebar lists sessions in reverse chronological order

The SPA SHALL render a sidebar listing the user's chat sessions, ordered by `last_active_at DESC` (most recent at the top). Each row SHALL display: the session `title` (populated from the first exchange by the service — the static placeholder `New chat` is rendered only while the title is still empty), a legible timestamp combining an absolute local date-time with a relative recency label (e.g. `10-16 14:03 · 3 hr ago`) at a font size no smaller than 11px, and a click target that loads the session's messages into the main chat panel. A title written or refined after the list was fetched SHALL appear on the next list refresh (run terminal, session adoption, or dialog reopen) without a page reload.

#### Scenario: Sidebar shows the user's sessions

- **WHEN** the SPA mounts and the user has 3 sessions
- **THEN** the sidebar renders 3 rows in reverse chronological order; each row is clickable

#### Scenario: Soft-deleted sessions are absent

- **WHEN** the SPA fetches the session list after the user deleted a session
- **THEN** the deleted session does not appear in the sidebar

#### Scenario: Populated titles distinguish rows

- **WHEN** the user has 3 sessions that each received a first message
- **THEN** each row renders its stored title instead of `New chat`, and a session with no messages still shows the `New chat` placeholder

#### Scenario: Rows show an absolute and relative timestamp

- **WHEN** a session's `last_active_at` is 3 hours ago
- **THEN** its row renders the absolute local date-time and a relative label (e.g. `3 hr ago`) at a legible size; a session older than a year still renders its absolute date

### Requirement: Lazy-load pagination

The sidebar SHALL initially render the first 10 sessions (page size configurable via `PYTHON_AGENT_SESSIONS_PAGE_SIZE`, default 10, max 50). When the user scrolls within 200px of the bottom of the list, the sidebar SHALL fetch the next page and append it. While a page is in flight, the existing rows SHALL NOT re-render. When no more pages exist (`next_cursor=null`), the lazy-load SHALL NOT trigger further fetches.

#### Scenario: Initial mount fetches one page

- **WHEN** the SPA mounts and the user has 25 sessions
- **THEN** the sidebar renders the first 10 and a sentinel div at the bottom; the sentinel triggers the next-page fetch when the user scrolls within 200px

#### Scenario: End of list stops fetching

- **WHEN** the last page returns `next_cursor=null`
- **THEN** the sentinel div is removed (no further IntersectionObserver triggers)

#### Scenario: Concurrent fetches do not duplicate rows

- **WHEN** the user scrolls fast enough to trigger two IntersectionObserver events in flight
- **THEN** the second fetch is dropped while the first is in flight (single-flight per page index)

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

### Requirement: Tab reload restores the active session

When the SPA reloads the page, the sidebar SHALL restore the active session id from `sessionStorage`, and SHALL re-fetch the messages for that session. If the restored session id no longer exists (deleted by the user or reaped by retention), the SPA SHALL fall back to an empty welcome state without showing an error to the user.

#### Scenario: Reload restores the active session

- **WHEN** the user reloads the SPA after starting a session
- **THEN** the sidebar highlights the same session row and the chat panel renders the same messages

#### Scenario: Reload after session deletion clears gracefully

- **WHEN** the user reloads after deleting the active session
- **THEN** the SPA renders the welcome state; no error toast or console error appears

### Requirement: Mobile sidebar drawer

The SPA SHALL render the sidebar as a collapsible drawer on viewports narrower than 768px (`md:` breakpoint). A hamburger button in the chat header SHALL toggle the drawer; the drawer SHALL auto-close when a session is clicked.

#### Scenario: Narrow viewport shows the drawer

- **WHEN** the viewport is 600px wide
- **THEN** the sidebar is hidden by default; a hamburger button appears; clicking it opens the drawer as an overlay; clicking a session in the drawer closes it

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

### Requirement: Viewport-bounded dialog with an internally scrolling rail

The chat dialog SHALL NOT grow beyond the viewport. The dialog's height SHALL stay fixed for every history length: an empty session and a session with many turns SHALL open at the same height, with the session rail and the message list scrolling internally. The message composer SHALL remain visible and reachable without scrolling the page (Radix keeps document scroll locked while the dialog is open). The mobile drawer presentation is unchanged.

#### Scenario: Long history scrolls the rail, not the dialog

- **WHEN** the user opens the chat dialog on an 800px-tall viewport with 30 sessions (multiple lazy-loaded pages appended)
- **THEN** the dialog's bounding box stays within the viewport, the rail's scroll container has `scrollHeight > clientHeight`, scrolling the rail moves rows, and the composer stays visible

#### Scenario: Short history shares the fixed height

- **WHEN** the user has 2 sessions and a short message history
- **THEN** the dialog opens at the same fixed height as a long history and the rail shows no scrollbar

#### Scenario: Message list scrolls independently

- **WHEN** a single session's messages exceed the message area's height
- **THEN** the message list scrolls internally while the rail and composer remain reachable
