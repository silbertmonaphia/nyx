# Spec Delta

## Purpose

When `AGENT_ENABLED=true`, makes the Go chat handler a thin SSE proxy that streams Python agent events to the existing frontend envelope byte-for-byte, so the frontend stays unchanged and the orchestrator can live in Python.

## ADDED Requirements

### Requirement: Proxy to Python service when agent enabled

When `AGENT_ENABLED=true`, the chat handler SHALL post the chat request body to `PYTHON_AGENT_URL/agent/run` (default `http://agent:9000/agent/run`), forwarding the user's `Authorization: Bearer <token>` header, and SHALL stream the response SSE body to the client. When `AGENT_ENABLED=false`, the existing single-turn path runs unchanged.

#### Scenario: AGENT_ENABLED=true proxies Python

- **WHEN** a chat request arrives and `AGENT_ENABLED=true`
- **THEN** the chat handler invokes the proxy and the client receives the Python service's response body byte-for-byte

#### Scenario: AGENT_ENABLED=false single-turn

- **WHEN** a chat request arrives and `AGENT_ENABLED=false`
- **THEN** the chat handler runs today's single-turn path; the proxy is never invoked

### Requirement: Wire envelope is byte-for-byte identical

The proxy SHALL NOT transform the SSE frames it receives from Python before forwarding them. The response status, response headers (`Content-Type: text/event-stream`, `Cache-Control: no-cache`, `Connection: keep-alive`, `X-Accel-Buffering: no`), and the SSE body (`event: <kind>\ndata: <json>\n\n`) MUST match the Python service's output exactly.

#### Scenario: Frontend sees same frames as before

- **WHEN** the Python service returns a stream containing `plan` / `step_start` / `tool_call` / `tool_result` / `step_end` / `delta` / `done` / `error` / `terminator` frames
- **THEN** the client receives each frame with the same `event:` line, same JSON payload, same terminator (`\n\n`) — no rewriting, no wrapping, no extra fields

#### Scenario: Old wire shape preserved when agent disabled

- **WHEN** `AGENT_ENABLED=false` and a chat runs through the single-turn path
- **THEN** the client sees the same `delta` / `done` / `note` / `error` / `terminator` frames it saw before any of this work landed

### Requirement: Python service unavailable fails closed

When the proxy cannot reach `PYTHON_AGENT_URL` (connection refused, DNS failure, timeout), the chat handler SHALL emit a single SSE `error{code:"agent_unavailable"}` frame followed by `done` + `terminator`, and SHALL return HTTP 200 (the SSE envelope was opened). The proxy MUST NOT silently fall back to the single-turn path — that would hide agent-side failures from the operator and from the user's `AGENT_ENABLED=true` expectation.

#### Scenario: Connection refused

- **WHEN** the proxy cannot establish a TCP connection to `PYTHON_AGENT_URL`
- **THEN** the chat handler emits `error{code:"agent_unavailable"}` and terminates the SSE stream

#### Scenario: Python returns 5xx

- **WHEN** the Python service returns HTTP 500
- **THEN** the chat handler emits `error{code:"agent_unavailable"}` and terminates

#### Scenario: Python returns 401 (JWT rejected)

- **WHEN** the Python service rejects the forwarded Bearer token with 401
- **THEN** the chat handler MUST NOT translate this into a different client error — the response is forwarded byte-for-byte (the operator's Python service is misconfigured; not a client issue)

### Requirement: Forward Bearer token

The proxy SHALL forward the user's `Authorization: Bearer <access_token>` header from the chat request to the Python service. The proxy MUST NOT use a service-to-service token, MUST NOT mint a new token, and MUST NOT log the token value.

#### Scenario: Token forwarded

- **WHEN** a chat request arrives with `Authorization: Bearer eyJhbGc...`
- **THEN** the proxy's HTTP client sends the same header on the request to `PYTHON_AGENT_URL/agent/run`

#### Scenario: Token never logged

- **WHEN** the proxy logs the request
- **THEN** the token value does not appear in any log line; only a fingerprint (e.g. first 6 chars + `…`) MAY appear for debugging

### Requirement: Single-turn fallback parity preserved

When `AGENT_ENABLED=false`, the chat handler MUST NOT call the proxy. The single-turn path's SSE output, including every existing event's bytes, MUST be byte-identical to the pre-change wire format. A regression test in `internal/chat/handler_test.go` SHALL assert this.

#### Scenario: Existing handler test unchanged

- **WHEN** `AGENT_ENABLED=false` and a chat request runs
- **THEN** every existing assertion in `internal/chat/handler_test.go` and `internal/chat/service_test.go` continues to pass byte-for-byte