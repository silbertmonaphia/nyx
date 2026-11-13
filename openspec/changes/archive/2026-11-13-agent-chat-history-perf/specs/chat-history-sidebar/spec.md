# Spec Delta

## MODIFIED Requirements

### Requirement: Click-to-replay

When the user clicks a session row in the sidebar, the SPA SHALL set the active session id, fetch the session's messages via `GET /agent/sessions/{id}/messages` using pagination (the most recent page — client-configurable page size, default 50 — plus `has_more`/`next_before`), and render them in `ChatPanel` in chronological order. The resume POST and the replay GET SHALL be issued concurrently: the replay MUST NOT wait for the resume, and a resume failure SHALL be logged without blocking or erroring the replay. While the initial replay fetch is in flight, the panel SHALL render loading skeleton rows rather than the welcome/empty state. When the replay renders fewer messages than the session holds (`has_more=true`), scrolling within a threshold of the top of the message list SHALL fetch the next older page via `next_before` and prepend it above the rendered list without moving the viewport content (scroll-position pinning), single-flight per page. If a token stream is live when the click lands, the stream SHALL be detached, not aborted: the SPA keeps the SSE connection open and drops its subsequent frames so the run completes server-side. The SSE stream for the next user message targets the active session id via the existing `run_id` echo.

#### Scenario: Replay renders messages in order

- **WHEN** the user clicks the 3rd session in the sidebar
- **THEN** `ChatPanel` detaches any live stream (the run continues in the background), fetches the messages, and renders them as user/assistant bubbles in chronological order; the assistant's `tool_calls` (if any) are rendered as the same "agent steps" collapsible region the live stream produces

#### Scenario: Replay shows a skeleton, not the welcome state

- **WHEN** the user clicks a session row and the replay fetch is in flight
- **THEN** the panel renders loading skeleton rows; the welcome/empty state is not shown, and the skeleton is replaced by the messages when the fetch lands

#### Scenario: Resume does not delay the replay

- **WHEN** the user clicks a session row while the resume endpoint is slow (or fails)
- **THEN** the replay fetch is already in flight from the same click, not queued behind the resume; a resume failure logs quietly and the replay still renders

#### Scenario: Scrolling up loads older messages without jumping

- **WHEN** a session holds more than one page and the user scrolls to the top of the message list
- **THEN** the SPA fetches the next older page via `next_before` and prepends it; the message the user was viewing stays in the viewport (no scroll jump)
- **AND** continued scrolling loads further pages until `has_more=false`, after which no further fetches fire

#### Scenario: Rapid scroll-up bursts do not duplicate pages

- **WHEN** the user scrolls in a way that triggers the load-older trigger repeatedly while a page fetch is in flight
- **THEN** the extra triggers are dropped (single-flight); the assembled list contains no duplicate message ids

#### Scenario: Clicking the active session is a no-op

- **WHEN** the user clicks the row of the currently-active session
- **THEN** nothing changes (no refetch, no scroll, no panel clear)

#### Scenario: Switching away mid-stream keeps the run alive

- **WHEN** the user clicks another session row while a run is streaming
- **THEN** the stream is detached rather than aborted, the clicked session's messages replay, and the detached run's session row shows the background-running indicator until the run reaches a terminal state

#### Scenario: A short session renders fully with no load-older affordance

- **WHEN** the user clicks a session whose message count fits within one page (`has_more=false`)
- **THEN** the full history renders and scrolling to the top triggers no fetch

## ADDED Requirements

### Requirement: Sidebar list refreshes on tab visibility

When the chat dialog is open and the browser tab becomes visible or focused, the SPA SHALL refetch page 1 of the session list and merge it into the rendered rail with the existing semantics: fresh rows replace matching rows in place (the server owns their order and counts), rows beyond page 1 keep their position, and no duplicate ids appear. The refresh SHALL be single-flight and throttled (at most one visibility-triggered fetch per 30 seconds; a manually triggered refresh or run-terminal refresh is unaffected). The SPA SHALL NOT poll on a timer: absent a visibility change, a run terminal, session adoption, deletion, or remount, the rail does not refetch. A failed visibility refresh SHALL be quiet — the existing list is retained, no error toast. Stale relative timestamps ("3 hr ago") on visible rows SHALL update as part of the refresh.

#### Scenario: Sessions from another tab appear on focus

- **WHEN** a session is created in another tab (or another device), and the user returns focus to this tab with the dialog open
- **THEN** the new session's row appears in the rail without a page reload, ranked by its `last_active_at`

#### Scenario: Rapid tab switches do not stack fetches

- **WHEN** the user switches away and back repeatedly within the throttle window
- **THEN** at most one visibility-triggered fetch is in flight; consecutive triggers inside the window are dropped and the rail renders no duplicates

#### Scenario: Failed refresh is quiet

- **WHEN** the visibility-triggered fetch fails (network hiccup, agent restart)
- **THEN** the existing rows stay rendered, no error toast or console error appears, and the next trigger retries

#### Scenario: No polling while the tab stays visible

- **WHEN** the user leaves the tab visible and idle for several minutes with the dialog open
- **THEN** no periodic list fetches are issued (the rail only refetches on visibility, run terminal, adoption, deletion, or remount)
