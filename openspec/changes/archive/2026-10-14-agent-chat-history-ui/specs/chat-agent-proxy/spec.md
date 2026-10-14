# Spec Delta

## Purpose

(see existing capability)

## ADDED Requirements

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
