# Spec Delta

## ADDED Requirements

### Requirement: On-demand per-slot connectivity probe

The API SHALL expose `POST /api/me/llm-config/validate`, authenticated by Bearer token with the user id taken exclusively from the verified JWT claim, routed on the same feature gate as the other three config endpoints. The request body SHALL use the same eleven-field shape as PUT. The endpoint SHALL probe exactly those slots whose base URL is non-empty — main LLM, embedding, fallback, and Tavily — and SHALL report slots with no base URL as skipped without treating them as an error. A blank key field SHALL resolve to the stored key for a configured user (mirroring the PUT contract); for an unconfigured user a blank key on a remote endpoint SHALL be probed on the provider's keyless path where one exists (Tavily) and reported as `invalid_config` otherwise. The response SHALL carry only per-slot results and MUST NOT contain stored or submitted API keys. Probing SHALL NOT create or modify the stored configuration row.

#### Scenario: Wrong key is reported as authentication failure

- **WHEN** a user submits an embedding group whose endpoint responds `401` to the probe call
- **THEN** the embedding slot's result is `auth_failed` carrying upstream status `401`, and the other slots' results are unaffected

#### Scenario: Filled slots are each probed independently

- **WHEN** a user submits a main-LLM group whose endpoint is unreachable and a Tavily group whose key is valid
- **THEN** the response reports `unreachable` for the main-LLM slot and `ok` for the Tavily slot in the same response

#### Scenario: Empty slots are skipped, not errors

- **WHEN** a user submits only the embedding group with the other three sections empty
- **THEN** the chat, fallback, and Tavily slots are reported as skipped, and the response status is `200`

#### Scenario: Blank key uses the stored key

- **WHEN** a configured user submits their existing base URL and model with the key field left blank
- **THEN** the probe authenticates with the stored decrypted key, and the response contains no key material

#### Scenario: Unauthenticated access refused

- **WHEN** the endpoint is called without a valid Bearer token
- **THEN** the response is `401`

#### Scenario: Endpoint absent when the feature is disabled

- **WHEN** the deployment runs with the LLM feature disabled
- **THEN** the validate route is not registered, alongside the other config routes

### Requirement: Probe results are classified per slot

Each probed slot SHALL return a status from the fixed set `ok`, `auth_failed`, `rate_limited`, `invalid_model`, `invalid_config`, `width_mismatch`, `unreachable`, `timeout`, together with the upstream HTTP status code where the upstream produced one. Upstream `401` and `403` SHALL map to `auth_failed`; `429` SHALL map to `rate_limited`; `404` with a model-shaped cause SHALL map to `invalid_model`; an upstream `400` SHALL map to `invalid_config` (the probe's request shape is fixed, so a schema rejection means the submitted values do not fit the endpoint); a request that never reached the upstream (DNS, dial, TLS) SHALL map to `unreachable`; exceeding the probe timeout SHALL map to `timeout`; and an embedding vector whose width differs from the deployment's embedding width SHALL map to `width_mismatch`. Classification SHALL be derived from a typed carrier of the upstream status, not from parsing error strings. The fixed set and each slot's meaning SHALL be identical across slots.

#### Scenario: Upstream status is visible in the result

- **WHEN** the embedding endpoint answers `401` to the probe
- **THEN** the result carries `status: "auth_failed"` and `http_status: 401`, distinguishable from a network-level `unreachable`

#### Scenario: Embedding width mismatch is classified

- **WHEN** the embedding endpoint responds successfully but the returned vector's width differs from the deployment's embedding width
- **THEN** the embedding slot's result is `width_mismatch`, not `ok`

#### Scenario: Timeout is distinct from unreachable

- **WHEN** the upstream accepts the connection but does not respond within the probe timeout
- **THEN** the slot's result is `timeout`, not `unreachable`

### Requirement: Probes are bounded and side-effect free

Each probe request SHALL pass the same SSRF validation and honour `BYOK_ALLOW_PRIVATE_URL` with the same semantics as the runtime path. Each slot's probe SHALL run under a bounded, configured timeout (default 10 seconds, independent of the router probe timeout). The endpoint SHALL be rate limited per user. Probe outcomes SHALL be logged with the slot name and outcome only — never an API key, authorization header, or upstream response body — and probe results SHALL NOT be cached.

#### Scenario: Private endpoint follows the runtime gate

- **WHEN** a user with `BYOK_ALLOW_PRIVATE_URL=false` submits a loopback base URL and requests a probe
- **THEN** the probe behaves exactly as a runtime request to that URL would under the same configuration, and the classification reflects the SSRF refusal

#### Scenario: Keys never appear in logs

- **WHEN** a probe is executed with a distinctive key value against a logging-enabled deployment
- **THEN** no log line emitted during the probe contains that value

#### Scenario: Probe amplification is bounded

- **WHEN** a single user calls the validate endpoint repeatedly in excess of its per-user rate limit
- **THEN** excess calls are rejected with `429` and trigger no upstream request

### Requirement: Config modal exposes per-section test-connection controls

The config modal SHALL provide a test-connection control per visible section (main LLM, embedding, Tavily), each probing only its own section's form values (with a blank key field resolved against the stored key as the API defines) without saving. A section's result SHALL render inline within that section, localized to the UI language, with a distinct state per status class: success, credential failure, network failure, and configuration problem. While a section's probe is in flight that section's control SHALL indicate the in-flight state and further clicks SHALL be suppressed, and other sections' controls SHALL remain usable. An empty section's control SHALL render no result (nothing probed), not a failure. The controls SHALL be independent of the save action — saving remains possible whether or not a probe has been run, and a probe MUST NOT write the configuration. Probing one section SHALL NOT send requests for any other section, so a mid-edit value in another section is never probed and no other provider's quota is spent.

#### Scenario: Wrong key shows a credential error inline

- **WHEN** the user fills the embedding section with a key the provider rejects and clicks that section's test-connection control
- **THEN** the embedding section shows an inline credential-failure message naming the authentication failure and the upstream status, in the UI language, and no request is sent for any other section

#### Scenario: Sections probe independently

- **WHEN** the embedding section's probe is in flight and the user clicks the main-LLM section's control
- **THEN** the main-LLM probe executes, and neither probe's outcome affects the other's rendering

#### Scenario: Correct configuration shows success

- **WHEN** the user clicks a section's test-connection control with values that provider accepts
- **THEN** that section shows an inline success state

#### Scenario: Probe does not save

- **WHEN** the user clicks any section's test-connection control and then closes the modal without saving
- **THEN** the stored configuration is unchanged, and reopening the modal shows the previously stored values

### Requirement: The probe discloses exactly what it sent

Each probed slot's result SHALL include the exact outbound request the probe made: HTTP method, full target URL, request headers, and request body. Every credential value in the headers SHALL be redacted to the established tail-4 masking convention (scheme preserved, key body elided); the plaintext key MUST NOT appear in the response, the rendered page, or the copy output. The disclosure SHALL reflect what actually left the server, not a reconstruction — including the keyless access-mode header on a keyless Tavily probe. The modal SHALL render the disclosure collapsibly inside the section that produced it and SHALL offer a copy action producing a replayable command in which the credential is an explicit placeholder the user substitutes themselves.

#### Scenario: The shown request is the real one

- **WHEN** the embedding section's probe completes successfully
- **THEN** the disclosed method, URL, headers, and body match the request the upstream actually received, with the authorization header shown as scheme plus tail-4 only

#### Scenario: Copy output carries a placeholder, not the key

- **WHEN** the user invokes the copy action on a disclosed request
- **THEN** the produced command contains the full URL and body with the credential replaced by an explicit placeholder, and a search for the plaintext key in the produced text finds nothing

#### Scenario: Keyless probe discloses its access mode

- **WHEN** a Tavily probe runs with no key and sends the keyless access-mode header
- **THEN** the disclosure shows that header verbatim, since it is exactly what was sent
