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

#### Scenario: User deletion cascades

- **WHEN** a user account is deleted
- **THEN** their configuration row is removed with it

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

### Requirement: Strict BYOK — no server-env fallback on user paths

When a user has no configuration row, every user-facing LLM path SHALL be unavailable for that user: chat returns 409 before streaming; RAG retrieval and feed embedding-indexing resolve no embedder. Server-level `LLM_*` environment configuration SHALL NOT serve as a fallback for any user's request. Clearing the configuration SHALL re-gate these paths immediately.

#### Scenario: Clearing re-gates

- **WHEN** a configured user deletes their configuration and then sends a chat message
- **THEN** the chat request fails with the not-configured error, without calling any model

### Requirement: In-modal guidance names the provider, where to obtain credentials, and the affected features

The config modal SHALL show, per section (main LLM, embedding, Tavily, optional retry model), in both UI languages: one example placeholder per empty input (URL and model name), a short description of what the group drives, where to obtain the credentials (provider sign-up/console URLs, plus the local-vLLM option for the LLM and embedding groups), and the note that all eight required fields must be present before AI Agent becomes usable. The embedding section SHALL additionally state the deployment's embedding-dimension requirement (1024) with an example compatible model. The optional retry-model section SHALL state that leaving it empty only affects the single-turn chat fallback path, not agent runs.

#### Scenario: First-time user sees actionable guidance

- **WHEN** a user with no configuration opens the modal from the blocking gate
- **THEN** each section displays its example placeholders, credential-source description (e.g. Tavily console URL and its free tier), and the feature-impact note, localized to the UI language

#### Scenario: Embedding constraint is visible before saving

- **WHEN** the embedding section is rendered
- **THEN** the 1024-dimension requirement and an example compatible model are shown alongside the input

### Requirement: Config forwarded to the agent per run, never client-supplied

When an authenticated, configured user's chat request is proxied to the Python agent, the Go handler SHALL include the user's decrypted LLM configuration and Tavily configuration in the `/agent/run` payload. These fields MUST NOT be accepted from the client request body, MUST NOT be logged, and the fallback fields MUST NOT be forwarded.

#### Scenario: Payload carries the owner's config only

- **WHEN** a chat request is proxied
- **THEN** the payload's `llm_config`/`tavily_config` are the JWT-identified user's stored values, and a client-supplied config field in the chat request body is rejected by the existing strict body decoding

#### Scenario: Config never logged

- **WHEN** any log line, trace span attribute, or error detail is emitted while handling the run
- **THEN** no API key plaintext appears in it
