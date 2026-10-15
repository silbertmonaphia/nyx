# Spec Delta

## ADDED Requirements

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
