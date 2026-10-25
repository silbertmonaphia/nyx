# Agent Rag Bridge Specification

## Purpose

Lets the Python agent service invoke Go RAG without reimplementing it — a thin authenticated HTTP endpoint that wraps `rag.Service.Retrieve` so per-owner scoping and the safe-error funnel stay in one place.

## Requirements

### Requirement: Endpoint exposes rag.Service.Retrieve

The service SHALL expose `GET /internal/rag/retrieve` accepting query parameters `query` (string, required) and `user_id` (int64, required). The endpoint SHALL return HTTP 200 with JSON `{"context_block": "<rendered context>", "citations": [{"feed_id": <int>, "snippet": "<text>"}]}` on success, HTTP 401 when no Bearer token is present, HTTP 400 when required params are missing.

#### Scenario: Authenticated retrieval

- **WHEN** a request arrives with `Authorization: Bearer <valid token>` and `?query=hello&user_id=42`
- **THEN** the endpoint reads the user identifier from the verified JWT (NOT from the query param), invokes `rag.Service.Retrieve(ctx, userIDFromJWT, query)`, and returns the JSON envelope

#### Scenario: Query param user_id ignored

- **WHEN** a request arrives with `?user_id=999` but the JWT's user identifier is `42`
- **THEN** the endpoint uses the JWT's `42` for the RAG call; the query param's `999` is ignored

#### Scenario: Missing token rejected

- **WHEN** a request arrives without `Authorization: Bearer …`
- **THEN** the endpoint returns HTTP 401 and does not invoke `rag.Service.Retrieve`

#### Scenario: Missing query rejected

- **WHEN** a request arrives without `query=`
- **THEN** the endpoint returns HTTP 400 with a static safe detail

### Requirement: Per-owner scoping enforced server-side

The endpoint MUST NOT honour a user identifier supplied by the caller — only the verified JWT's claim. Any RAG failure that depends on per-row ownership (cross-user retrieval) SHALL surface as `feed.ErrNotFound`-equivalent (single 404, no existence leak).

#### Scenario: Cross-user query returns empty

- **WHEN** the JWT's user id has no feeds that match the query
- **THEN** the endpoint returns HTTP 200 with `{"context_block": "", "citations": []}` — not a 404, not an error

### Requirement: Safe-error funnel

Errors SHALL be translated through `api.ClassifyAndLog(ctx, err, "RAG retrieval failed")`. Internal error strings (SQL fragments, embedding errors, panic traces) MUST NOT appear in the response body.

#### Scenario: Internal error hidden

- **WHEN** `rag.Service.Retrieve` returns a wrapped internal error
- **THEN** the endpoint returns a static safe detail; the underlying error is logged with the request ID

### Requirement: No new public API surface

The endpoint SHALL be mounted on the existing chi router, behind the existing JWT middleware. The path prefix `/internal/` SHALL signal "service-to-service" — never linked from frontend, never in OpenAPI / Swagger UI documentation.

#### Scenario: Endpoint not advertised

- **WHEN** a client fetches `/api/swagger/doc.json`
- **THEN** the `/internal/rag/retrieve` path is not present in the OpenAPI document

### Requirement: Unconfigured user retrieval degrades to the empty envelope

When the authenticated user has no configuration row OR no embedding group, the RAG bridge SHALL return HTTP 200 with `{"context_block": "", "citations": []}` — byte-identical to the no-matches envelope — without contacting any embedding endpoint. This outcome MUST be indistinguishable to the Python caller from an empty corpus, and MUST NOT leak whether the user is unconfigured or merely has no matching feeds.

#### Scenario: Unconfigured user queries the bridge

- **WHEN** a user with no configuration row, or with no embedding group, reaches the RAG bridge through an agent run
- **THEN** the bridge returns the standard empty envelope and no embedder endpoint is dialed

#### Scenario: Retrieval still scoped after configuration

- **WHEN** a configured user queries the bridge
- **THEN** retrieval embeds the query with the user's own embedding configuration and searches only that user's feeds, as today

### Requirement: An embedding of unexpected width degrades to the empty envelope

When the user's embedding endpoint returns a vector whose width differs from the deployment's embedding width, the bridge SHALL return the same 200-empty envelope as the no-matches case and MUST NOT fail the request with a server error. The width check SHALL happen before the vector search, so no search is issued against a mismatched vector.

#### Scenario: Mismatched embedding width returns the empty envelope

- **WHEN** a configured user's embedding endpoint returns a vector whose width differs from the deployment's embedding width
- **THEN** the bridge returns the standard empty envelope with a warning naming the mismatch, and no vector search is issued

#### Scenario: Matching embedding width searches normally

- **WHEN** a configured user's embedding endpoint returns a vector of the deployment's embedding width
- **THEN** the bridge searches the user's feeds and returns the matching passages
