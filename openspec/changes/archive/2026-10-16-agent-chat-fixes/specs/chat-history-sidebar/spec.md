# Spec Delta

## MODIFIED Requirements

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

## ADDED Requirements

### Requirement: Viewport-bounded dialog with an internally scrolling rail

The chat dialog SHALL NOT grow beyond the viewport. With an arbitrarily long session history (tens of sessions across lazy-loaded pages), the dialog's height SHALL stay within the viewport, the session rail SHALL scroll internally when its rows exceed the available height, and the message composer SHALL remain visible and reachable without scrolling the page (Radix keeps document scroll locked while the dialog is open). The mobile drawer presentation is unchanged; a short history SHALL still size the dialog to its content.

#### Scenario: Long history scrolls the rail, not the dialog

- **WHEN** the user opens the chat dialog on an 800px-tall viewport with 30 sessions (multiple lazy-loaded pages appended)
- **THEN** the dialog's bounding box stays within the viewport, the rail's scroll container has `scrollHeight > clientHeight`, scrolling the rail moves rows, and the composer stays visible

#### Scenario: Short history is unaffected

- **WHEN** the user has 2 sessions and a short message history
- **THEN** the dialog sizes to its content as before and the rail shows no scrollbar

#### Scenario: Message list still scrolls independently

- **WHEN** a single session's messages exceed the message area's cap
- **THEN** the message list scrolls internally (its existing bounded behavior) while the rail and composer remain reachable
