# Spec Delta

## MODIFIED Requirements

### Requirement: Forward Bearer token

The proxy SHALL send a per-run delegated access token, minted for the verified caller with the user id and username from the authenticated request context, as `Authorization: Bearer <delegated_token>` on the request to the Python service. The delegated token SHALL be signed with the deployment's `JWT_SECRET`, carry the same user claims as the caller's access token, and use the lifetime configured by `AGENT_RUN_TOKEN_TTL` (default `1h`) rather than the caller's access-token lifetime. The proxy MUST NOT forward the caller's original access token, MUST NOT repurpose `Authorization` for the service credential, and MUST NOT log either token value. It SHALL also stamp `X-Nyx-Service-Token` from `AGENT_SERVICE_TOKEN` on every request to the Python service. When the delegated token cannot be minted, the handler MUST fail closed with `error{code:"agent_unavailable"}` and MUST NOT call the Python service.

#### Scenario: Token forwarded

- **WHEN** a chat request arrives with `Authorization: Bearer eyJhbGc...`
- **THEN** the proxy's HTTP client sends `Authorization: Bearer <newly_minted_token>` on the request to `PYTHON_AGENT_URL/agent/run`, where the new token carries the verified user id and username and a later `exp` than the caller's token

#### Scenario: Service token stamped

- **WHEN** the proxy sends any request to the Python service
- **THEN** the request carries `X-Nyx-Service-Token` equal to the configured `AGENT_SERVICE_TOKEN`, alongside the delegated `Authorization` bearer

#### Scenario: Token never logged

- **WHEN** the proxy logs the request
- **THEN** neither the delegated token nor the service token value appears in any log line; only a fingerprint (e.g. first 6 chars + `…`) of the delegated token MAY appear for debugging

#### Scenario: Caller token not forwarded

- **WHEN** the proxy sends the run request
- **THEN** the caller's original access token does not appear on the outbound request

#### Scenario: Mint failure fails closed

- **WHEN** the delegated token cannot be minted
- **THEN** the handler emits `error{code:"agent_unavailable"}` followed by the terminal frames, and the Python service receives no request
