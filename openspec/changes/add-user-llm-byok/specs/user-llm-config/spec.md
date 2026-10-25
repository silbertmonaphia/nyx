# Spec Delta

## Purpose

Per-user BYOK LLM configuration: each user stores their own model endpoints, API keys, and model names; the backend and agent consume the requesting user's values at request time; keys are encrypted at rest and never returned in plaintext.

## ADDED Requirements

### Requirement: Per-user settings persisted and encrypted at rest

The system SHALL persist one configuration row per user holding: main LLM base URL, API key, and model; embedding base URL, API key, and model; Tavily search base URL and API key (all eight required); and optional retry-model LLM base URL, API key, and model. Every API key SHALL be encrypted at rest with AES-256-GCM under a server-side key, stored as versioned ciphertext (`v1:` prefix), and the row SHALL be deleted when its user is deleted.

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

PUT SHALL accept all eleven fields (fallback optional as a group), validate URLs parse as http/https with a host and no userinfo, reject placeholder API keys, and upsert the row. On create, a missing required key SHALL be rejected; on update, an empty key field SHALL mean "keep the stored key". The fallback fields SHALL be all-or-nothing: a partial fallback section SHALL be rejected. User-supplied URLs MAY point at private or localhost addresses (self-hosted deployment).

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

### Requirement: Each slot reaches its own endpoint

A user's main-LLM base URL and embedding base URL SHALL be honoured independently: chat traffic reaches the main-LLM endpoint and embedding traffic reaches the embedding endpoint. Resolving a user's configuration SHALL produce a client per slot — the embedding client is built from the embedding base URL, API key, and model, not reused from the chat client.

#### Scenario: Embedding traffic uses the embedding endpoint

- **WHEN** a configured user's feed is indexed or their RAG query is embedded
- **THEN** the embedding request is sent to the user's embedding base URL with the user's embedding model

#### Scenario: Chat traffic uses the main endpoint

- **WHEN** a configured user sends a chat message
- **THEN** the chat request is sent to the user's main-LLM base URL with the user's main model

### Requirement: Strict BYOK — no server-env fallback on user paths

When a user has no configuration row, every user-facing LLM path SHALL be unavailable for that user: chat returns 409 before streaming; RAG retrieval and feed embedding-indexing resolve no embedder. Server-level `LLM_*` environment configuration SHALL NOT serve as a fallback for any user's request. Clearing the configuration SHALL re-gate these paths immediately.

#### Scenario: Clearing re-gates

- **WHEN** a configured user deletes their configuration and then sends a chat message
- **THEN** the chat request fails with the not-configured error, without calling any model

### Requirement: In-modal guidance names the provider, where to obtain credentials, and the affected features

The config modal SHALL show, per section (main LLM, embedding, Tavily, optional retry model), in both UI languages: one example placeholder per empty input (URL and model name), a short description of what the group drives, where to obtain the credentials (provider sign-up/console URLs, plus the local-vLLM option for the LLM and embedding groups), and the note that all eight required fields must be present before AI Agent becomes usable. Both base-URL fields SHALL state that the value is an OpenAI-compatible endpoint whose path carries the provider's version prefix (e.g. a trailing `/v1`), and the placeholders SHALL show that prefix. The embedding section SHALL additionally state the deployment's embedding-dimension requirement (1024) with an example compatible model, and note that a model emitting a different width is rejected at query time rather than at save time. The main-LLM section SHALL note that the configured model must exist on the configured endpoint (the deployment does not probe at save time), that an endpoint that needs no credential still requires a non-placeholder value, and that the retry-model section, when left empty, only affects the single-turn chat fallback path and not agent runs. The search section SHALL note that the slot is shaped for the deployment's search provider.

#### Scenario: First-time user sees actionable guidance

- **WHEN** a user with no configuration opens the modal from the blocking gate
- **THEN** each section displays its example placeholders, credential-source description (e.g. Tavily console URL and its free tier), the base-URL path convention, and the feature-impact note, localized to the UI language

#### Scenario: Embedding constraint is visible before saving

- **WHEN** the embedding section is rendered
- **THEN** the 1024-dimension requirement and an example compatible model are shown alongside the input

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
