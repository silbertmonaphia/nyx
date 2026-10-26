# Spec Delta

## ADDED Requirements

### Requirement: Service token gates every route except health

The service SHALL require `X-Nyx-Service-Token` on every HTTP route except the exact path `/healthz`. A missing or incorrect value MUST be rejected with HTTP 401 and body `{"detail":"unauthorized"}` before any handler runs. The comparison SHALL be constant-time, and the header value MUST NOT appear in logs or responses. `/metrics` on the FastAPI port SHALL be gated; the separate Prometheus exporter port is unaffected.

#### Scenario: Missing token rejected

- **WHEN** a request arrives at any path other than `/healthz` without `X-Nyx-Service-Token`
- **THEN** the service returns HTTP 401 with body `{"detail":"unauthorized"}` and the handler does not run

#### Scenario: Wrong token rejected

- **WHEN** a request arrives with a `X-Nyx-Service-Token` value that does not match the configured token
- **THEN** the service returns HTTP 401 with body `{"detail":"unauthorized"}` and the handler does not run

#### Scenario: Health probe stays open

- **WHEN** a request arrives at exactly `/healthz` without a service token
- **THEN** the health handler runs and its normal 200/503 response is returned

#### Scenario: Metrics on the application port are gated

- **WHEN** a request arrives at `/metrics` on the FastAPI port without a valid service token
- **THEN** the service returns HTTP 401 and metrics are not exposed

#### Scenario: Valid token reaches the handler

- **WHEN** a request arrives with the correct service token
- **THEN** the middleware forwards it to the route handler unchanged

### Requirement: New env key for service token

The service SHALL read the unprefixed `AGENT_SERVICE_TOKEN` environment variable (other settings use the `PYTHON_AGENT_` prefix) and SHALL refuse to start when it is unset or shorter than 32 bytes. The Go backend MUST be configured with the same value. There is no insecure default.

#### Scenario: Missing token refuses startup

- **WHEN** the service starts without `AGENT_SERVICE_TOKEN`
- **THEN** it fails to start with an error naming `AGENT_SERVICE_TOKEN`; no port is bound

#### Scenario: Short token refuses startup

- **WHEN** `AGENT_SERVICE_TOKEN` is set to fewer than 32 bytes
- **THEN** it fails to start with an error naming `AGENT_SERVICE_TOKEN`

#### Scenario: Valid token starts the service

- **WHEN** `AGENT_SERVICE_TOKEN` is set to at least 32 bytes
- **THEN** the service starts normally

## MODIFIED Requirements

### Requirement: Caller supplies verified user id via header

The service SHALL require the `X-Nyx-User-Id` request header on `/agent/run` and SHALL reject any request without it with HTTP 400 before invoking the orchestrator. The value MUST parse as an integer; non-integer values are rejected with HTTP 400. The Python service SHALL NOT verify JWTs — the service-token middleware authenticates the calling backend, and `X-Nyx-User-Id` is the verified-user assertion made by that authenticated caller. The `Authorization` bearer token is forwarded as a passthrough to the MCP and RAG bridges, which re-verify the JWT themselves.

#### Scenario: Missing header rejected

- **WHEN** `POST /agent/run` arrives without `X-Nyx-User-Id`
- **THEN** the service returns HTTP 400 with `detail` mentioning the header name and the orchestrator does not run

#### Scenario: Non-integer header value rejected

- **WHEN** `POST /agent/run` arrives with `X-Nyx-User-Id` that does not parse as an integer
- **THEN** the service returns HTTP 400 with `detail` explaining the requirement

#### Scenario: User id read from header, not body

- **WHEN** a request arrives with a valid `X-Nyx-User-Id` header
- **THEN** the orchestrator uses that value (not anything from the request body) for per-owner RAG retrieval, tool dispatch via MCP, and the budget counter key

### Requirement: Approval endpoint accepts decisions under the verified-user-header contract

The service SHALL expose `POST /agent/approvals` accepting `{"run_id": ..., "approval_id": ..., "decision": "approve" | "deny"}`. The endpoint SHALL require the `X-Nyx-User-Id` header exactly like `/agent/run` (behind the service-token middleware; no JWT verification) and MUST reject a missing or non-integer header with HTTP 400 before any state change. The endpoint SHALL verify via the run metadata layer that the run belongs to the header user, and MUST reject non-owned runs with the same response as an unknown run (single sentinel, no existence leak). The first decision recorded for an approval id wins; duplicates SHALL be accepted idempotently (HTTP 200) without changing the recorded outcome. A decision for an unknown or already-expired approval id MUST be rejected with a typed error response (HTTP 404) and MUST never create an approval. The response MUST NOT carry internal error text.

#### Scenario: Missing or non-integer user header rejected

- **WHEN** `POST /agent/approvals` arrives without `X-Nyx-User-Id`, or with a non-integer value
- **THEN** the service returns HTTP 400 with a `detail` naming the header, and no decision is recorded

#### Scenario: Non-owned run is indistinguishable from unknown

- **WHEN** the header user id does not own the run named in the request
- **THEN** the response equals the response for an unknown run and no decision is recorded

#### Scenario: First decision wins, duplicates are idempotent

- **WHEN** an `approve` is recorded for an approval id and a later `deny` arrives for the same id
- **THEN** the later request returns HTTP 200 and the recorded outcome remains `approve`

#### Scenario: Unknown or expired approval rejected

- **WHEN** a decision arrives for an approval id that does not exist or whose TTL has expired
- **THEN** the service returns HTTP 404 and records nothing
