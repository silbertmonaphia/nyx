# Spec Delta

## MODIFIED Requirements

### Requirement: Caller supplies verified user id via header

The service SHALL require the `X-Nyx-User-Id` request header on `/agent/run` and SHALL reject any request without it with HTTP 400 before invoking the orchestrator. The value MUST parse as an integer; non-integer values are rejected with HTTP 400. The Python service SHALL NOT verify JWTs — the service-token middleware authenticates the calling backend, and `X-Nyx-User-Id` is the verified-user assertion made by that authenticated caller. The `Authorization` bearer token is a per-run delegated access token minted by the Go backend for the verified user; it is forwarded as a passthrough to the MCP and RAG bridges, which re-verify the JWT themselves. Its lifetime is independent of the caller's access-token expiry.

#### Scenario: Missing header rejected

- **WHEN** `POST /agent/run` arrives without `X-Nyx-User-Id`
- **THEN** the service returns HTTP 400 with `detail` mentioning the header name and the orchestrator does not run

#### Scenario: Non-integer header value rejected

- **WHEN** `POST /agent/run` arrives with `X-Nyx-User-Id` that does not parse as an integer
- **THEN** the service returns HTTP 400 with `detail` explaining the requirement

#### Scenario: User id read from header, not body

- **WHEN** a request arrives with a valid `X-Nyx-User-Id` header
- **THEN** the orchestrator uses that value (not anything from the request body) for per-owner RAG retrieval, tool dispatch via MCP, and the budget counter key

#### Scenario: Delegated bearer outlives the caller token

- **WHEN** a run is still executing after the caller's access token has expired
- **THEN** the MCP and RAG bridges still accept the delegated bearer presented by the orchestrator
