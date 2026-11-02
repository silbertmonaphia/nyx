# Agent Rag Bridge Specification

## Purpose

Lets the Python agent service invoke Go RAG without reimplementing it — a thin authenticated HTTP endpoint that wraps `rag.Service.Retrieve` so per-owner scoping and the safe-error funnel stay in one place.

## Requirements

### Requirement: Endpoint exposes rag.Service.Retrieve

The service SHALL expose `GET /internal/rag/retrieve` with required `query` and `user_id` parameters, returning the rendered context envelope on success and the documented status codes on failure. It SHALL accept an optional corpus selector and knowledge scope parameters; citations SHALL carry a corpus discriminator and corpus-specific identity fields.

#### Scenario: Response envelope carries tagged citations

- **WHEN** a retrieval request succeeds
- **THEN** the body is `{"context_block": "<rendered context>", "citations": [...]}`, `citations` is always a JSON array and never `null`, a feed citation carries `kind: "feed"` and its `feed_id`, and a knowledge citation carries `kind: "doc"`, `feed_id: 0`, `doc_id`, `source_name`, `rel_path`, `heading_path`, and `locator`

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

#### Scenario: Absent corpus behaves as the feed corpus

- **WHEN** a request arrives with `?query=hello` and no `corpus` parameter
- **THEN** the endpoint searches the feed corpus and returns the same passages and citation shape it returned before the knowledge corpus existed

#### Scenario: Knowledge corpus selected

- **WHEN** a request arrives with `?query=hello&corpus=kb`
- **THEN** the endpoint searches only the knowledge corpus and every returned citation carries `kind` `doc` together with its document identity, source name, relative path, heading path, and locator

#### Scenario: Both corpora selected

- **WHEN** a request arrives with `?query=hello&corpus=both`
- **THEN** the endpoint returns the combined feed and knowledge passages, fused by rank, each citation carrying the `kind` that identifies its corpus

#### Scenario: Narrowing parameters reach the knowledge search

- **WHEN** a request arrives with `?query=hello&corpus=kb&source=3&path=B-Sport/`
- **THEN** the endpoint searches the knowledge corpus restricted to that source and that path prefix

#### Scenario: Unknown corpus value is refused

- **WHEN** a request arrives with an unrecognized `corpus` value
- **THEN** the endpoint returns HTTP 400 with a static safe detail and performs no search

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

### Requirement: The knowledge corpus is retrieved through the same bridge and the same funnel

A knowledge search SHALL be performed by the Go backend on behalf of the authenticated caller, SHALL scope its results to that caller, and SHALL be subject to the same safe-error funnel as a feed search: an internal failure SHALL be logged with the request identifier and SHALL NOT surface an internal error string in the response body.

#### Scenario: Knowledge retrieval failure yields the empty envelope

- **WHEN** the knowledge search fails internally
- **THEN** the endpoint returns the standard empty envelope and the internal error is logged with the request identifier rather than returned

#### Scenario: Knowledge retrieval is scoped to the caller

- **WHEN** a knowledge search runs on behalf of an authenticated caller
- **THEN** it searches only the shared source and that caller's own private sources

### Requirement: The bridge exposes a knowledge outline for the caller's visible corpus

The bridge SHALL expose a read-only endpoint that returns, for each source visible to the authenticated caller, the source identifier, display name, scope, document count, chunk count, and top-level directory names. The response SHALL contain no document content and no absolute filesystem path, and SHALL list no source that is not visible to the caller.

#### Scenario: Outline lists the visible sources

- **WHEN** the outline endpoint is called with a valid Bearer token
- **THEN** it lists the shared source and the caller's own private sources with their counts and top-level directory names, and no other user's private source

#### Scenario: Outline is not advertised publicly

- **WHEN** a client fetches `/api/swagger/doc.json`
- **THEN** neither the outline path nor the retrieval path appears in the OpenAPI document
