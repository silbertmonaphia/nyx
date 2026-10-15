# Spec Delta

## Purpose

Lets users see and replay their past chat sessions via a ChatGPT-style sidebar in the SPA. The sidebar lists sessions in reverse chronological order with lazy-load, click-to-load replay into `ChatPanel`, and a `+ New chat` button. The wire contract (`/api/chat` request body `run_id`, SSE frames) is unchanged; the sidebar is a passive reader of the Slice 1 storage via the agent-side REST endpoints.

## ADDED Requirements

### Requirement: Sidebar lists sessions in reverse chronological order

The SPA SHALL render a sidebar listing the user's chat sessions, ordered by `last_active_at DESC` (most recent at the top). Each row SHALL display: the session `title` (or the static placeholder `New chat` when the title is empty — the list response carries no message snippet, so a derived label would cost a per-row messages fetch), a relative timestamp (`5 min ago`, `3 days ago`), and a click target that loads the session's messages into the main chat panel.

#### Scenario: Sidebar shows the user's sessions

- **WHEN** the SPA mounts and the user has 3 sessions
- **THEN** the sidebar renders 3 rows in reverse chronological order; each row is clickable

#### Scenario: Soft-deleted sessions are absent

- **WHEN** the SPA fetches the session list after the user deleted a session
- **THEN** the deleted session does not appear in the sidebar

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

When the user clicks a session row in the sidebar, the SPA SHALL set the active session id, fetch the session's messages via `GET /agent/sessions/{id}/messages`, and render them in `ChatPanel` in chronological order. The current token stream (if any) is cleared; the SSE stream for the next user message targets the active session id via the existing `run_id` echo.

#### Scenario: Replay renders messages in order

- **WHEN** the user clicks the 3rd session in the sidebar
- **THEN** `ChatPanel` clears any live stream, fetches the messages, and renders them as user/assistant bubbles in chronological order; the assistant's `tool_calls` (if any) are rendered as the same "agent steps" collapsible region the live stream produces

#### Scenario: Clicking the active session is a no-op

- **WHEN** the user clicks the row of the currently-active session
- **THEN** nothing changes (no refetch, no scroll, no panel clear)

### Requirement: Plus-New-chat button

The SPA SHALL render a `+ New chat` button in the chat header (next to the existing chat controls). When clicked, the SPA SHALL POST `/agent/sessions` (via the Go proxy chain), receive the new session id, set it as the active session id, clear the live stream, and reset the chat panel to an empty state. Subsequent user messages are recorded under the new session. The rail SHALL NOT be refetched on create — the new session's row appears on the next list fetch (reload or the next lazy-load trigger), keeping the interaction single-round-trip.

#### Scenario: New chat clears the panel

- **WHEN** the user clicks `+ New chat`
- **THEN** the sidebar highlights no row (no active session), `ChatPanel` clears the live stream and renders an empty welcome state; the next user message starts a fresh session

#### Scenario: New chat keeps the sidebar list

- **WHEN** the user clicks `+ New chat`
- **THEN** the existing session rows remain in the sidebar (no scroll-to-top, no list clear); only the active row changes

### Requirement: Tab reload restores the active session

When the SPA reloads the page, the sidebar SHALL restore the active session id from `sessionStorage` (the same persistence mechanism the existing `tokenStore` uses), and SHALL re-fetch the messages for that session. If the restored session id no longer exists (deleted by the user or reaped by retention), the SPA SHALL fall back to an empty welcome state without showing an error to the user.

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
