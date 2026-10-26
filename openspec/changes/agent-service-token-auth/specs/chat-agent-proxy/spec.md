# Spec Delta

## ADDED Requirements

### Requirement: Backend degrades agent routes when the agent path is misconfigured

When `AGENT_ENABLED=true` but `PYTHON_AGENT_URL` is unset or `AGENT_SERVICE_TOKEN` is missing or shorter than 32 bytes, the Go API SHALL start and keep non-agent features (auth, feeds, MCP) available. The agent-backed routes (`POST /api/chat`, `/api/chat/sessions*`, `/api/chat/approvals`) SHALL respond HTTP 503 with the standard error envelope whose `details` is a static safe string naming the misconfiguration. The handler MUST NOT silently fall back to the single-turn chat path. When `AGENT_ENABLED=false`, the single-turn path runs and no token is required. The key SHALL be bound explicitly from the environment (the process's env-key allowlist), not inferred. The token value MUST NOT be logged or returned.

#### Scenario: Missing token leaves non-agent features up

- **WHEN** the service starts with `AGENT_ENABLED=true`, a valid `PYTHON_AGENT_URL`, and no `AGENT_SERVICE_TOKEN`
- **THEN** the API binds and serves auth/feeds normally, and every agent-backed route returns 503 with a static misconfiguration detail

#### Scenario: Short token leaves non-agent features up

- **WHEN** `AGENT_ENABLED=true` and `AGENT_SERVICE_TOKEN` is shorter than 32 bytes
- **THEN** the API binds and every agent-backed route returns 503 (no silent single-turn fallback)

#### Scenario: Missing URL leaves non-agent features up

- **WHEN** `AGENT_ENABLED=true` and `PYTHON_AGENT_URL` is unset
- **THEN** the API binds and every agent-backed route returns 503 (no silent single-turn fallback)

#### Scenario: Agent disabled needs no token

- **WHEN** `AGENT_ENABLED=false` and `AGENT_SERVICE_TOKEN` is unset
- **THEN** the service starts normally on the single-turn chat path

## MODIFIED Requirements

### Requirement: Python service unavailable fails closed

When the proxy cannot reach `PYTHON_AGENT_URL` (connection refused, DNS failure, timeout), the chat handler SHALL emit a single SSE `error{code:"agent_unavailable"}` frame followed by `done` + `terminator`, and SHALL return HTTP 200 (the SSE envelope was opened). The proxy MUST NOT silently fall back to the single-turn path — that would hide agent-side failures from the operator and from the user's `AGENT_ENABLED=true` expectation.

#### Scenario: Connection refused

- **WHEN** the proxy cannot establish a TCP connection to `PYTHON_AGENT_URL`
- **THEN** the chat handler emits `error{code:"agent_unavailable"}` and terminates the SSE stream

#### Scenario: Python returns 5xx

- **WHEN** the Python service returns HTTP 500
- **THEN** the chat handler emits `error{code:"agent_unavailable"}` and terminates

#### Scenario: Python returns 401 (JWT rejected)

- **WHEN** the Python service returns HTTP 401 for any reason, including a missing or mismatched `X-Nyx-Service-Token` or an upstream authentication rejection it surfaces
- **THEN** the chat handler MUST NOT translate this into a different client error — the response is forwarded byte-for-byte (the deployment is misconfigured; not a client issue)

### Requirement: Forward Bearer token

The proxy SHALL forward the user's `Authorization: Bearer <access_token>` header from the chat request to the Python service. It SHALL also stamp `X-Nyx-Service-Token` from `AGENT_SERVICE_TOKEN` on every request to the Python service. The proxy MUST NOT repurpose `Authorization` for the service credential, MUST NOT mint a new user token, and MUST NOT log either token value.

#### Scenario: Token forwarded

- **WHEN** a chat request arrives with `Authorization: Bearer eyJhbGc...`
- **THEN** the proxy's HTTP client sends the same header on the request to `PYTHON_AGENT_URL/agent/run`

#### Scenario: Service token stamped

- **WHEN** the proxy sends any request to the Python service
- **THEN** the request carries `X-Nyx-Service-Token` equal to the configured `AGENT_SERVICE_TOKEN`, alongside the unchanged `Authorization` passthrough

#### Scenario: Token never logged

- **WHEN** the proxy logs the request
- **THEN** neither the user token nor the service token value appears in any log line; only a fingerprint (e.g. first 6 chars + `…`) of the user token MAY appear for debugging

### Requirement: Session REST routes proxied to the agent

The Go chat package SHALL register the following authenticated REST routes under the same `/api/chat` route group (auth middleware first; no SSE headers — these are plain JSON):

- `GET /api/chat/sessions` — list the caller's sessions (query params `limit`, `before` forwarded verbatim)
- `GET /api/chat/sessions/{session_id}/messages` — replay one session
- `POST /api/chat/sessions` — create an empty session
- `POST /api/chat/sessions/{session_id}/resume` — refresh the session's activity timestamp
- `DELETE /api/chat/sessions/{session_id}` — soft-delete a session

Each route SHALL forward to the Python agent at `PYTHON_AGENT_URL` under the matching `/agent/sessions...` path with the verified `X-Nyx-User-Id` header (from `reqctx`), the user's `Authorization` header as a passthrough, and `X-Nyx-Service-Token` carrying `AGENT_SERVICE_TOKEN`. The response status and body SHALL be forwarded verbatim (JSON in, JSON out; no SSE). Query strings SHALL be forwarded without transformation.

When `AGENT_ENABLED=false` or `PYTHON_AGENT_URL` is unset, the routes SHALL respond `503` with the standard error envelope (`{"error": "...", "details": "..."}`) — sessions do not exist in the single-turn world; this is not a client error.

When the agent is unreachable (connection refused / DNS failure / timeout), the routes SHALL respond `502` with the standard error envelope; the underlying error is logged via `api.ClassifyAndLog` and the static safe detail reaches the wire.

#### Scenario: Authenticated list proxies through

- **WHEN** an authenticated client calls `GET /api/chat/sessions?limit=10`
- **THEN** the handler sends `GET <PYTHON_AGENT_URL>/agent/sessions?limit=10` with `X-Nyx-User-Id` set to the verified user id and `X-Nyx-Service-Token` set to `AGENT_SERVICE_TOKEN`, and the client receives the agent's response status and JSON body unchanged

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

When `AGENT_ENABLED=true`, the Go API SHALL expose `POST /api/chat/approvals`, authenticated with the project's standard Bearer JWT middleware (same as every protected chat route). The handler SHALL forward the request body to `PYTHON_AGENT_URL/agent/approvals`, stamping the verified user id as `X-Nyx-User-Id` and `X-Nyx-Service-Token` from `AGENT_SERVICE_TOKEN`, and SHALL NOT log either token value. The upstream status and body SHALL be passed through unchanged; the handler MUST NOT interpret or shortcut a decision. When the agent proxy is disabled or unreachable, the route SHALL fail closed with an error response — an approval MUST never be auto-granted, and the failure MUST NOT fall back to any other path.

#### Scenario: Authenticated decision is forwarded

- **WHEN** an authenticated user posts `{"run_id": ..., "approval_id": ..., "decision": "approve"}` and the Python service accepts it
- **THEN** the client receives the Python service's status and body, and the forwarded request carried the verified user id in `X-Nyx-User-Id` and the service credential in `X-Nyx-Service-Token`

#### Scenario: Missing or invalid JWT is rejected at the edge

- **WHEN** the route is called without a valid Bearer token
- **THEN** the request is rejected with the standard authentication error and is never forwarded to the Python service

#### Scenario: Agent unreachable fails closed

- **WHEN** the Python service is unreachable or the proxy is disabled
- **THEN** the route returns an error response (no implicit success), and no approval is recorded anywhere

#### Scenario: Token value never logged

- **WHEN** the handler logs the approval request
- **THEN** neither the user token value nor the service token value appears in any log line
