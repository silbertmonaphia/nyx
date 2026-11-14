# User Llm Config Specification

## Purpose

Per-user BYOK LLM configuration: each user stores their own model endpoints, API keys, and model names; the backend and agent consume the requesting user's values at request time; keys are encrypted at rest and never returned in plaintext; an on-demand per-slot connectivity probe validates the configured endpoints without saving.

## Requirements

### Requirement: Per-user settings persisted and encrypted at rest

The system SHALL persist one configuration row per user holding: optional main LLM base URL, API key, and model; optional embedding base URL, API key, and model; optional Tavily search base URL and API key; and optional retry-model LLM base URL, API key, and model. At least one group must carry a value on save. Every API key SHALL be encrypted at rest with AES-256-GCM under a server-side key, stored as versioned ciphertext (`v1:` prefix), and the row SHALL be deleted when its user is deleted.

#### Scenario: Row survives across sessions

- **WHEN** a user saves their configuration and later opens the config modal from another browser session
- **THEN** the non-key fields and masked key previews come back from the stored row

#### Scenario: Ciphertext at rest

- **WHEN** the `user_llm_settings` row is inspected directly in the database
- **THEN** each API-key column contains `v1:`-prefixed AES-GCM ciphertext, never the plaintext key

#### Scenario: Encryption key validated at boot

- **WHEN** the backend starts with `LLM_ENABLED=true` and `USER_LLM_ENCRYPTION_KEY` is missing or shorter than 32 bytes
- **THEN** startup is refused with an error naming the variable

#### Scenario: Row is removed with its user row

- **WHEN** the `users` row for a configured user is deleted at the database level
- **THEN** the `user_llm_settings` row is removed with it, by foreign-key cascade

### Requirement: Config CRUD scoped to the authenticated identity

The API SHALL expose `GET /api/me/llm-config`, `PUT /api/me/llm-config`, and `DELETE /api/me/llm-config`, authenticated by Bearer token, with the user id taken exclusively from the verified JWT claim (never from any input field). GET on an unconfigured user SHALL return `{"configured": false}` with null sections. GET responses SHALL carry masked keys (tail-4 characters only) and never plaintext keys. DELETE SHALL be idempotent.

#### Scenario: Unconfigured GET

- **WHEN** a user with no saved configuration calls GET
- **THEN** the response is `200 {"configured": false, "llm": null, "embedding": null, "tavily": null, "fallback": null}`

#### Scenario: Configured GET masks keys

- **WHEN** a configured user calls GET
- **THEN** each section carries its base URL, model (where applicable), and an `api_key_masked` showing at most the tail 4 characters

#### Scenario: Cross-user isolation

- **WHEN** user A has saved a configuration and user B calls GET, PUT, or DELETE
- **THEN** B sees `configured: false`, and neither B's reads, writes, nor deletes can observe or affect A's row

#### Scenario: Unauthenticated access refused

- **WHEN** any of the three endpoints is called without a valid Bearer token
- **THEN** the response is 401

#### Scenario: Endpoints exist only with the LLM feature enabled

- **WHEN** the deployment runs with the LLM feature disabled
- **THEN** the three config endpoints are not routed, and the chat route is absent on the same gate

### Requirement: PUT validates and upserts

PUT SHALL accept all eleven fields (Tavily and fallback optional as groups), validate supplied URLs parse as http/https with a host and no userinfo, reject placeholder API keys, and upsert the row. On create, a missing required key SHALL be rejected UNLESS the corresponding base URL's host is loopback (`localhost`, `127.0.0.0/8`, `::1`) — a local model server needs no credential, and the stored key column stays empty, so no placeholder value is ever persisted. On update, an empty key field SHALL mean "keep the stored key". The fallback fields SHALL be all-or-nothing on base URL and model; a blank fallback key is acceptable only when the fallback base URL is loopback or a stored key exists. User-supplied URLs MAY point at private or localhost addresses (self-hosted deployment).

#### Scenario: Create requires all eight

- **WHEN** a PUT omits any required main-LLM, embedding, or Tavily field (including its API key)
- **THEN** the response is 400 naming the problem and no row is written

#### Scenario: Update with blank key keeps the stored key

- **WHEN** a configured user PUTs with the key fields left empty
- **THEN** the stored ciphertext keys are preserved and the non-key fields are updated

#### Scenario: Fallback all-or-nothing

- **WHEN** a PUT supplies only some of the three fallback fields
- **THEN** the response is 400 and no row is written

#### Scenario: Non-http scheme rejected

- **WHEN** a PUT supplies a base URL whose scheme is not http or https
- **THEN** the response is 400

#### Scenario: Placeholder key rejected

- **WHEN** a PUT supplies a placeholder key such as `changeme` or `sk-xxx`
- **THEN** the response is 400

#### Scenario: Private endpoint accepted

- **WHEN** a PUT supplies `http://localhost:8000/v1` as a base URL
- **THEN** the configuration is accepted

#### Scenario: Omitting embedding is accepted

- **WHEN** a PUT omits the embedding section (or sends it empty)
- **THEN** the configuration is accepted, the stored embedding fields are empty, and GET reports `embedding: null`; RAG retrieval returns the empty envelope and feed indexing is skipped

#### Scenario: Embedding width is probed on save

- **WHEN** a PUT supplies an embedding group whose endpoint is reachable and returns a vector whose width differs from the deployment's embedding width
- **THEN** the response is 400 naming `embedding_model` and no configuration is stored

#### Scenario: Unreachable embedding endpoint does not block the save

- **WHEN** the embedding endpoint cannot be reached during the save-time probe
- **THEN** the configuration is saved and the failure is logged; the runtime width guard still prevents a mismatched vector from being used

#### Scenario: Embedding model is immutable after the first save

- **WHEN** a configured user submits a different embedding model while the embedding group remains configured
- **THEN** the response is 400 naming `embedding_model`, and the stored configuration is unchanged; the base URL and API key MAY still be updated

#### Scenario: Partial embedding group is rejected

- **WHEN** a PUT supplies a base URL without a model, or a model/key without a base URL
- **THEN** the response is 400 naming the missing field

#### Scenario: Omitting Tavily is accepted

- **WHEN** a PUT omits the Tavily section (or sends it empty)
- **THEN** the configuration is accepted, the stored Tavily fields are empty, and GET reports `tavily: null`

#### Scenario: Tavily key without a base URL is rejected

- **WHEN** a PUT supplies a Tavily API key but no Tavily base URL
- **THEN** the response is 400 naming `tavily_base_url`

#### Scenario: Tavily keyless tier is accepted

- **WHEN** a PUT supplies a non-loopback Tavily base URL with an empty key
- **THEN** the configuration is accepted (the search tool uses the provider's keyless tier)

#### Scenario: Loopback endpoint needs no key

- **WHEN** a PUT supplies a loopback base URL for a section and leaves its API key empty
- **THEN** the configuration is accepted and the stored key for that section is empty (no placeholder secret is persisted)

#### Scenario: Remote endpoint still requires a key

- **WHEN** a PUT supplies a non-loopback base URL and leaves its API key empty
- **THEN** the response is 400 naming that key field

### Requirement: Each slot reaches its own endpoint

A user's main-LLM base URL and embedding base URL SHALL be honoured independently: chat traffic reaches the main-LLM endpoint and embedding traffic reaches the embedding endpoint. Resolving a user's configuration SHALL produce a client per slot — the embedding client is built from the embedding base URL, API key, and model, not reused from the chat client.

#### Scenario: Embedding traffic uses the embedding endpoint

- **WHEN** a configured user's feed is indexed or their RAG query is embedded
- **THEN** the embedding request is sent to the user's embedding base URL with the user's embedding model

#### Scenario: Chat traffic uses the main endpoint

- **WHEN** a configured user sends a chat message
- **THEN** the chat request is sent to the user's main-LLM base URL with the user's main model

### Requirement: Strict BYOK — no server-env fallback on user paths

The AI Agent SHALL require the main LLM group: with no configuration row OR no main-LLM group, chat returns 409 before streaming. RAG retrieval and feed embedding-indexing SHALL depend only on the embedding group, so a user with embedding configured but no main LLM still indexes and retrieves; a user with neither simply gets the empty envelope and skipped indexing. Server-level `LLM_*` environment configuration SHALL NOT serve as a fallback for any user's request. Clearing the configuration SHALL re-gate these paths immediately. A configuration row containing only optional groups SHALL be saved successfully and the AI Agent panel SHALL remain gated.

#### Scenario: Clearing re-gates

- **WHEN** a configured user deletes their configuration and then sends a chat message
- **THEN** the chat request fails with the not-configured error, without calling any model

### Requirement: In-modal guidance names the provider, where to obtain credentials, and the affected features

The config modal SHALL show, per section (main LLM, embedding, Tavily), in both UI languages: one example placeholder per empty input (URL and model name), a short description of what the group drives, where to obtain the credentials (provider sign-up/console URLs, plus the local-vLLM option for the LLM and embedding groups), a link to that provider's own documentation, and the note that every group is optional — the main LLM enables the AI Agent while feeds and the rest of the app work without it — and that API keys may be left empty for localhost / 127.0.0.1 endpoints. The optional Embedding and Tavily sections SHALL each be collapsible (closed by default, auto-expanded when configured) and MUST show a red warning stating the concrete impact of leaving them empty: without Embedding the agent cannot retrieve the user's feeds (RAG / semantic search); without Tavily the agent will not plan web searches; chat and planning keep working either way. Each section's documentation link and guidance SHALL stand on its own — the embedding group is a separate provider on a separate endpoint with a separate failure mode, and its guidance MUST NOT be folded into the main-LLM group's text.

The main-LLM section SHALL state the three provider capabilities AI Agent depends on — streaming chat completions, OpenAI-shaped tool calls, and structured (`json_schema`) output — and what is lost when each is missing: without tool calls the AI Agent is unavailable and only the single-turn chat path works, and without structured output the planner falls back to its legacy free-text path. That section SHALL also state that the base URL is an OpenAI-compatible endpoint whose path carries the provider's version prefix (e.g. a trailing `/v1`) with the placeholder showing that prefix, that the configured model must exist on the configured endpoint (the deployment does not probe at save time), and that an endpoint needing no credential still requires a non-placeholder value.

The embedding section SHALL state, in its own text and behind its own documentation link, that the value must be a service exposing OpenAI-compatible embeddings (not every chat provider offers them, so this slot may point at a different service than the main LLM), that the deployment's embedding width is 1024 with an example compatible model, and that a model emitting a different width is refused at query time rather than at save time.

The retry-model group SHALL be accepted by the API but MUST NOT be exposed in the config modal in v1 (the agent has no retry-model semantics yet; the Go single-turn fallback remains supported for direct clients and future re-exposure). The search section SHALL state that the slot is shaped for the deployment's search provider.

#### Scenario: First-time user sees actionable guidance

- **WHEN** a user with no configuration opens the modal from the blocking gate
- **THEN** each section displays its example placeholders, credential-source description (e.g. Tavily console URL and its free tier), its own provider-documentation link, and the feature-impact note, localized to the UI language, with the Tavily impact rendered as a red warning

#### Scenario: Main-LLM section states what AI Agent needs from the provider

- **WHEN** the main-LLM section is rendered
- **THEN** it lists the streaming, tool-call, and structured-output capabilities and says what the user loses when the provider lacks each, next to the base-URL input

#### Scenario: Embedding constraint is visible before saving

- **WHEN** the embedding section is rendered
- **THEN** the 1024-dimension requirement, an example compatible model, the separate-provider note, and its own documentation link are shown alongside the input, without reference to the main-LLM section's capability list

### Requirement: Config forwarded to the agent per run, never client-supplied

When an authenticated, configured user's chat request is proxied to the Python agent, the Go handler SHALL include the user's decrypted LLM configuration and Tavily configuration in the `/agent/run` payload. These fields MUST NOT be accepted from the client request body, MUST NOT be logged, and the fallback fields MUST NOT be forwarded. The plaintext API key MUST NOT be written to durable storage by the agent runtime — in particular it MUST NOT travel inside the checkpointed graph state — and MUST NOT be attached to a trace span or log line.

#### Scenario: Payload carries the owner's config only

- **WHEN** a chat request is proxied
- **THEN** the payload's `llm_config`/`tavily_config` are the JWT-identified user's stored values, and a client-supplied config field in the chat request body is rejected by the chat endpoint's strict body decoding

#### Scenario: Config never logged

- **WHEN** any log line, trace span attribute, or error detail is emitted while handling the run
- **THEN** no API key plaintext appears in it

#### Scenario: Key is not persisted by the agent runtime

- **WHEN** an agent run completes and the agent's checkpoint tables and trace export are inspected
- **THEN** no API key plaintext appears in either

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

Each probed slot SHALL return a status from the fixed set `ok`, `auth_failed`, `rate_limited`, `invalid_model`, `invalid_config`, `width_mismatch`, `unreachable`, `timeout`, together with the upstream HTTP status code where the upstream produced one. Upstream `401` and `403` SHALL map to `auth_failed`; `429` SHALL map to `rate_limited`; `404` with a model-shaped cause SHALL map to `invalid_model`, and on a slot without model semantics (Tavily) a `404` SHALL map to `invalid_config` (the endpoint path is wrong); an upstream `400` SHALL map to `invalid_config` (the probe's request shape is fixed, so a schema rejection means the submitted values do not fit the endpoint); a request that never reached the upstream (DNS, dial, TLS) SHALL map to `unreachable`; exceeding the probe timeout SHALL map to `timeout`; and an embedding vector whose width differs from the deployment's embedding width SHALL map to `width_mismatch`. Classification SHALL be derived from a typed carrier of the upstream status, not from parsing error strings. The fixed set and each slot's meaning SHALL be identical across slots.

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

Each probe request SHALL pass the same SSRF validation and honour `BYOK_ALLOW_PRIVATE_URL` with the same semantics as the runtime path. Probe base URLs SHALL be validated with the same scheme allowlist (http/https) and userinfo rejection the PUT path applies, so a credential embedded in the URL is rejected as `invalid_config` rather than echoed in the disclosure. Each slot's probe SHALL run under a bounded, configured timeout (default 10 seconds, independent of the router probe timeout). The endpoint SHALL be rate limited per user. Probe outcomes SHALL be logged with the slot name and outcome only — never an API key, authorization header, or upstream response body — and probe results SHALL NOT be cached.

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
