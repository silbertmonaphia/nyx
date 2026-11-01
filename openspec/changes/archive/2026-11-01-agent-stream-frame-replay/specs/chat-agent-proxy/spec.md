# Spec Delta

## Purpose

Lets the SPA's resume cursor reach the agent through the Go edge without changing the byte-for-byte response contract.

## MODIFIED Requirements

### Requirement: Re-attach route is proxied to the agent

The proxy SHALL expose `GET /api/chat/runs/{run_id}/stream` inside the existing chat route group, behind the same bearer-auth middleware as the streaming POST and charged against the same per-user stream-rate limiter (a re-attach is a long-lived upstream stream — the same expense class as a run, not a cheap session read). The handler SHALL forward the path's `run_id` segment verbatim (never minted, rewritten, or locally validated, matching the POST body's `run_id` posture, URL-encoded on the outbound path), stamp the shared service token and the verified user's id from the request context as `X-Nyx-User-Id`, and forward the caller's `Authorization` header as a passthrough. The handler SHALL also forward the caller's `Last-Event-ID` header verbatim when present, and SHALL preserve a `last_event_id` query parameter on the outbound path, so a replay cursor reaches the agent unmodified. On HTTP 2xx from the agent the response SHALL use the standard SSE header set and the upstream body SHALL be forwarded byte-for-byte with a flush per chunk — including comment (`: keep-alive`) frames and `id:` fields — under the same wall-clock stream cap and the same no-double-termination guard as the POST path. The agent being disabled or misconfigured SHALL answer HTTP 503 with the static misconfigured envelope; the agent being unreachable SHALL answer HTTP 502. Agent error statuses other than 5xx SHALL be forwarded verbatim (the re-attach endpoint's typed 409/501 envelopes reach the client unchanged). A client disconnect cancels the upstream connection exactly as on the POST path; the run's lifetime is owned by the agent per its own specification.

#### Scenario: Live run re-attach proxies through

- **WHEN** an authenticated user calls `GET /api/chat/runs/<live run id>/stream`
- **THEN** the agent receives the verbatim run id, the service token, the `X-Nyx-User-Id` header, and the caller's Authorization header, and the SSE body — replay frames, keep-alives, live deltas, terminal trio — is forwarded byte-for-byte with a flush per chunk

#### Scenario: Replay cursor is forwarded

- **WHEN** the route is called with a `Last-Event-ID` header, or with a `last_event_id` query parameter
- **THEN** the agent receives it unmodified and the replayed frames carry their original `id:` fields

#### Scenario: Stream cap and double-terminate guard apply

- **WHEN** the configured stream cap expires mid-reattach, or expires after the upstream already forwarded its terminal frames
- **THEN** the handler synthesizes `error{code:"stream_timeout"}` + `done` + `terminator` in the first case and appends nothing in the second — identical to the POST path

#### Scenario: Agent disabled or unreachable

- **WHEN** the chat service has no agent proxy configured, or the agent is unreachable
- **THEN** the client receives HTTP 503 (static misconfigured envelope) or HTTP 502 respectively, before any SSE bytes

#### Scenario: Typed non-live envelope is forwarded verbatim

- **WHEN** the agent answers the re-attach with its 409 `run_not_live` or 501 `reattach_unavailable` envelope
- **THEN** the status and body reach the client unchanged, and the SPA's ladder keys on them

#### Scenario: Unauthenticated request rejected before the proxy

- **WHEN** the route is called without a valid bearer token
- **THEN** the request is rejected by the group's auth middleware with no outbound call

#### Scenario: Per-user stream limiter applies

- **WHEN** a user opens more concurrent re-attach streams than the per-user stream bucket allows
- **THEN** the excess streams are 429ed by the same limiter that governs `POST /api/chat`
