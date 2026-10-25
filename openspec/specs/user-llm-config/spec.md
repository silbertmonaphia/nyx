# User Llm Config Specification

## Purpose

Per-user BYOK LLM configuration: each user stores their own model endpoints, API keys, and model names; the backend and agent consume the requesting user's values at request time; keys are encrypted at rest and never returned in plaintext.

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
