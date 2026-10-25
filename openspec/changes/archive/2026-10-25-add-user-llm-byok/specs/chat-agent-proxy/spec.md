# Spec Delta

## Purpose

Per-user BYOK wiring for the chat path: the `/agent/run` proxy payload carries the requesting user's resolved LLM/Tavily configuration; unconfigured users are refused with a plain-JSON 409 before any SSE byte; single-turn parity is scoped to configured users.

## MODIFIED Requirements

### Requirement: Proxy to Python service when agent enabled

When `AGENT_ENABLED=true`, the chat handler SHALL post the chat request body to `PYTHON_AGENT_URL/agent/run` (default `http://agent:9000/agent/run`), forwarding the user's `Authorization: Bearer <token>` header, and SHALL stream the response SSE body to the client. The proxied body SHALL additionally carry the JWT-identified user's resolved LLM configuration and Tavily configuration (assembled server-side from the stored, decrypted row — never from client input); when the user has no stored configuration or no main-LLM group the request SHALL be refused with 409 and the proxy SHALL NOT be invoked. When `AGENT_ENABLED=false`, the existing single-turn path runs unchanged over the user's resolved provider.

#### Scenario: AGENT_ENABLED=true proxies Python with user config

- **WHEN** a chat request arrives from a user with a main LLM configured and `AGENT_ENABLED=true`
- **THEN** the chat handler invokes the proxy, the proxied body carries the user's `llm_config` and `tavily_config`, and the client receives the Python service's response body byte-for-byte

#### Scenario: AGENT_ENABLED=false single-turn

- **WHEN** a chat request arrives from a user with a main LLM configured and `AGENT_ENABLED=false`
- **THEN** the chat handler runs the single-turn path against the user's resolved provider; the proxy is never invoked

### Requirement: Single-turn fallback parity preserved

When `AGENT_ENABLED=false` and the requesting user has a main LLM configured, the chat handler MUST NOT call the proxy, and the single-turn path's SSE output — including every existing event's bytes — MUST be wire-identical to the pre-change format (the model endpoint and credentials differ per user; the frames do not). A regression test in `internal/chat/handler_test.go` SHALL assert this. The byte-for-byte guarantee is scoped to users with a main LLM: a user without one receives the not-configured 409 instead of a stream.

#### Scenario: Existing handler test unchanged

- **WHEN** `AGENT_ENABLED=false` and a chat request runs for a user with a stored configuration
- **THEN** every existing assertion in `internal/chat/handler_test.go` and `internal/chat/service_test.go` continues to pass byte-for-byte

## ADDED Requirements

### Requirement: Unconfigured users are refused before streaming

A chat request from a user with no stored LLM configuration or no main-LLM group SHALL be answered with HTTP 409, a JSON error envelope (never `text/event-stream`), and a machine-readable marker `llm_not_configured` carried in the envelope's `details` field, before any SSE byte is written and before any model endpoint is contacted. The envelope SHALL be the deployment's standard error envelope, whose `code` field carries the numeric HTTP status (409) and whose `error` field carries the human-readable message. Chat SHALL NOT use any other 409 status, so clients MAY key on the status alone.

#### Scenario: Unconfigured chat gets JSON 409

- **WHEN** a user with no stored configuration or no main-LLM group sends a chat message
- **THEN** the response is a `409` JSON envelope with `Content-Type: application/json`, `code` 409, and `details` `llm_not_configured`

#### Scenario: Config resolution failure is a 500, not a 409

- **WHEN** the configuration store itself fails while resolving the user's row
- **THEN** the response is a 500 envelope with the failure logged under the request ID, and no model endpoint is contacted

### Requirement: A user's fallback provider is liveness-probed at the right path

When a user configures a retry-model fallback, the single-turn path SHALL probe the primary provider's models endpoint before issuing the request, and the probe URL SHALL be derived from the primary's configured base URL without duplicating the provider's version path segment. A provider whose base URL already carries the version prefix SHALL be probed at `{base}/models`; a provider whose base URL does not SHALL be probed at `{base}/v1/models`. A failed probe SHALL fail the request over to the user's fallback provider.

#### Scenario: Version-prefixed base URL is probed without a duplicated prefix

- **WHEN** a user configures a main base URL ending in `/v1` together with a fallback
- **THEN** the probe targets the base URL with a single version path segment, and a healthy primary is used for the request instead of being treated as unreachable

#### Scenario: Unprefixed base URL keeps the version path

- **WHEN** a user configures a main base URL without a version path segment together with a fallback
- **THEN** the probe targets the base URL with the version path segment appended
