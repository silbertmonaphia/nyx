# Chat Agent Proxy Specification

## Purpose

When `AGENT_ENABLED=true`, makes the Go chat handler a thin SSE proxy that streams Python agent events to the existing frontend envelope byte-for-byte, so the frontend stays unchanged and the orchestrator can live in Python.

## Requirements

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

### Requirement: Request body forwards the optional run_id

The Go chat handler's `ChatRequest` SHALL accept an optional `run_id` field (uuid4 string, `json:"run_id,omitempty"`). When the client supplies it, the handler SHALL forward it unchanged as part of the proxied `/agent/run` body; the Python service owns validation (malformed uuid4 → its own 400). When the client omits it, the proxied body MUST NOT contain the field (`omitempty`), preserving today's fresh-run behaviour byte-for-byte. This unblocks the SPA echoing a session id for resume — before this change the field was rejected by `DisallowUnknownFields`.

The handler MUST NOT mint, rewrite, or validate run ids itself: the SPA (via the sidebar) and the Python service are the only parties that decide session identity.

#### Scenario: Supplied run_id round-trips to the agent

- **WHEN** a chat request arrives with `run_id` set to a valid uuid4 and `AGENT_ENABLED=true`
- **THEN** the proxied `/agent/run` body carries the same `run_id` value; the Python service resumes (or mints fresh if the id is not resumable) per its own resume contract

#### Scenario: Omitted run_id stays omitted

- **WHEN** a chat request arrives without `run_id`
- **THEN** the proxied body has no `run_id` key (the `omitempty` tag), byte-identical to the pre-change body for the same request

#### Scenario: Malformed run_id is forwarded, not rejected locally

- **WHEN** a chat request arrives with `run_id` that is not a uuid4
- **THEN** the handler forwards the value; the Python service's 400 response (with its `detail` naming the field) is what the client sees

### Requirement: Session REST routes proxied to the agent

The Go chat package SHALL register the following authenticated REST routes under the same `/api/chat` route group (auth middleware first; no SSE headers — these are plain JSON):

- `GET /api/chat/sessions` — list the caller's sessions (query params `limit`, `before` forwarded verbatim)
- `GET /api/chat/sessions/{session_id}/messages` — replay one session
- `POST /api/chat/sessions` — create an empty session
- `POST /api/chat/sessions/{session_id}/resume` — refresh the session's activity timestamp
- `DELETE /api/chat/sessions/{session_id}` — soft-delete a session

Each route SHALL forward to the Python agent at `PYTHON_AGENT_URL` under the matching `/agent/sessions...` path with the verified `X-Nyx-User-Id` header (from `reqctx`) and the user's `Authorization` header as a passthrough. The response status and body SHALL be forwarded verbatim (JSON in, JSON out; no SSE). Query strings SHALL be forwarded without transformation.

When `AGENT_ENABLED=false` or `PYTHON_AGENT_URL` is unset, the routes SHALL respond `503` with the standard error envelope (`{"error": "...", "details": "..."}`) — sessions do not exist in the single-turn world; this is not a client error.

When the agent is unreachable (connection refused / DNS failure / timeout), the routes SHALL respond `502` with the standard error envelope; the underlying error is logged via `api.ClassifyAndLog` and the static safe detail reaches the wire.

#### Scenario: Authenticated list proxies through

- **WHEN** an authenticated client calls `GET /api/chat/sessions?limit=10`
- **THEN** the handler sends `GET <PYTHON_AGENT_URL>/agent/sessions?limit=10` with `X-Nyx-User-Id` set to the verified user id, and the client receives the agent's response status and JSON body unchanged

#### Scenario: Unauthenticated request is rejected before the proxy

- **WHEN** a client calls any session route without a valid bearer token
- **THEN** the auth middleware responds 401 with the standard envelope; the proxy is never invoked

#### Scenario: Agent unreachable fails with 502

- **WHEN** the proxy cannot reach `PYTHON_AGENT_URL`
- **THEN** the handler responds `502` with the standard error envelope whose `details` is a static safe string (never `err.Error()`); the failure is logged with the request id

#### Scenario: Agent disabled responds 503

- **WHEN** `AGENT_ENABLED=false` and a client calls a session route
- **THEN** the handler responds `503` with the standard error envelope explaining the agent is disabled; no proxy call is attempted

#### Scenario: Cross-user session is a 404 from the agent, forwarded verbatim

- **WHEN** the agent responds `404` to a session route (owner-scope miss)
- **THEN** the client receives the same `404` and body; the Go layer does not translate, retry, or inspect it

### Requirement: Approval route forwards decisions to the Python service

When `AGENT_ENABLED=true`, the Go API SHALL expose `POST /api/chat/approvals`, authenticated with the project's standard Bearer JWT middleware (same as every protected chat route). The handler SHALL forward the request body to `PYTHON_AGENT_URL/agent/approvals`, stamping the verified user id as `X-Nyx-User-Id`, and SHALL NOT mint a service token or forward the user's token value to any log. The upstream status and body SHALL be passed through unchanged; the handler MUST NOT interpret or shortcut a decision. When the agent proxy is disabled or unreachable, the route SHALL fail closed with an error response — an approval MUST never be auto-granted, and the failure MUST NOT fall back to any other path.

#### Scenario: Authenticated decision is forwarded

- **WHEN** an authenticated user posts `{"run_id": ..., "approval_id": ..., "decision": "approve"}` and the Python service accepts it
- **THEN** the client receives the Python service's status and body, and the forwarded request carried the verified user id in `X-Nyx-User-Id`

#### Scenario: Missing or invalid JWT is rejected at the edge

- **WHEN** the route is called without a valid Bearer token
- **THEN** the request is rejected with the standard authentication error and is never forwarded to the Python service

#### Scenario: Agent unreachable fails closed

- **WHEN** the Python service is unreachable or the proxy is disabled
- **THEN** the route returns an error response (no implicit success), and no approval is recorded anywhere

#### Scenario: Token value never logged

- **WHEN** the handler logs the approval request
- **THEN** the token value does not appear in any log line

### Requirement: Over-long streams fail closed at a configurable cap

The proxy SHALL bound each upstream streaming call with a configurable wall-clock cap on the upstream request context — never a fixed client-level timeout that also bounds the streamed body, which severs healthy streams with no terminal frames. When the cap expires while the upstream stream is still live, the handler SHALL close the wire contract with a synthesized `error{code:"stream_timeout"}` frame followed by `done` and `terminator`, and the upstream request MUST be cancelled. The synthesized frames are a deliberate, bounded exception to byte-for-byte forwarding — sibling to the unreachable-agent branch — because the agent never got to emit its own terminator. When the upstream has already forwarded its own terminal frames (`done`, `terminator`, or the `[DONE]` sentinel) before the cap fires, the handler MUST NOT append any further frame: the stream is not double-terminated.

#### Scenario: Cap expires mid-stream

- **WHEN** the upstream stream is still live as the configured cap expires with the client attached
- **THEN** the client receives `error{code:"stream_timeout"}` + `done` + `terminator` on that connection and the upstream request is cancelled

#### Scenario: A healthy stream shorter than the cap is untouched

- **WHEN** the upstream stream completes normally before the cap
- **THEN** the body is forwarded byte-for-byte and no synthesized frame is appended

#### Scenario: No double-termination after the upstream's own terminal frames

- **WHEN** the cap expires after the upstream already forwarded its own terminal frames (its body held open past them)
- **THEN** nothing is appended and the client sees exactly the upstream bytes

#### Scenario: Client disconnect takes precedence

- **WHEN** the client disconnects before the cap expires
- **THEN** the upstream call is cancelled by the request context and no synthesized frame is written
