# agent-chat-titles Specification

## Purpose
Gives every chat session a meaningful title derived from its first exchange, so the history rail can distinguish conversations instead of rendering a column of identical "New chat" placeholders.

## Requirements

### Requirement: Deterministic title from the first user message

When the first user message of a session is recorded, the service SHALL set the session's `title` to a deterministic label derived from that message: whitespace collapsed to single spaces, trimmed, and truncated to at most 48 characters (counted in characters, never splitting a multi-byte character). The write MUST NOT overwrite a non-empty title, MUST NOT delay or alter the SSE stream, and MUST degrade silently when the chat store is unavailable (warn log; the run is unaffected).

#### Scenario: First message titles the session

- **WHEN** a fresh session's first user message "帮我推荐一家上海的生鲜电商，预算中等" is recorded
- **THEN** the session's `title` becomes that message (collapsed and within the length bound), and the next `GET /agent/sessions` returns it — the sidebar row no longer reads `New chat`

#### Scenario: Long messages truncate by characters

- **WHEN** the first user message is 200 CJK characters on one line
- **THEN** the stored title is the first 48 characters with no byte-splitting artifacts, and the recorded message content is unchanged

#### Scenario: Existing title is never overwritten

- **WHEN** a run starts on a session that already has a non-empty title (continuation or resumed run)
- **THEN** the title is unchanged and no title write fires

#### Scenario: Chat store unavailable degrades silently

- **WHEN** the title write fails because the chat store is unavailable
- **THEN** the run and its stream proceed unaffected, a WARN is logged, and the title stays empty (the rail shows `New chat`)

### Requirement: One-shot refinement from the first exchange

After the first assistant reply of a session has been recorded, the service SHALL asynchronously refine the session's title from the first exchange (first user message + first assistant reply) via a small LLM call on the configured planner model. The refinement SHALL follow the preference-extraction posture: fire-and-forget after the reply is recorded, never delaying, blocking, or altering the SSE stream; logging at WARN and leaving the deterministic title in place on any failure (LLM error, malformed output, missing store); no retry within the run; no consumption of the user's chat budget; at most once per session. A successful refinement SHALL overwrite the deterministic title in place, guarded by session ownership and `deleted_at IS NULL`.

#### Scenario: Refinement replaces the truncated title

- **WHEN** the first exchange completes and the refinement returns "生鲜电商推荐"
- **THEN** the session's `title` becomes "生鲜电商推荐"; the next sessions-list refresh reflects it in the sidebar

#### Scenario: Refinement failure keeps the deterministic title

- **WHEN** the refinement LLM call errors or times out
- **THEN** the chat completes normally, a WARN is logged, the deterministic title remains, and there is no retry within the run

#### Scenario: Slow refinement never delays the stream

- **WHEN** refinement takes 30 seconds while the reply stream completes in 2 seconds
- **THEN** `done` and `terminator` frames arrive at ~2 seconds, no frame carries refinement state, and the title updates later

#### Scenario: Refinement runs at most once per session

- **WHEN** the user sends a second, third, and fourth message in the same session
- **THEN** no further refinement call fires and the title stays stable

#### Scenario: Refinement never consumes the user chat budget

- **WHEN** the user is at their hourly chat budget cap
- **THEN** chat model calls are refused with `budget_exceeded` while the refinement (an internal operation) proceeds independent of the per-user budget counter

#### Scenario: Soft-deleted sessions are not refined

- **WHEN** the session is deleted before the refinement task writes
- **THEN** the title update is dropped (guarded by ownership and `deleted_at IS NULL`); no error surfaces
