# Spec Delta

## Purpose

Lets the Python agent service invoke Go RAG without reimplementing it — a thin authenticated HTTP endpoint that wraps `rag.Service.Retrieve` so per-owner scoping and the safe-error funnel stay in one place.

## MODIFIED Requirements

### Requirement: Endpoint exposes rag.Service.Retrieve

The service SHALL expose `GET /internal/rag/retrieve` accepting query parameters `query` (string, required) and `user_id` (int64, required). The endpoint SHALL return HTTP 200 with JSON `{"context_block": "<rendered context>", "citations": [...]}` on success, HTTP 401 when no Bearer token is present, HTTP 400 when required params are missing. The endpoint SHALL additionally accept an optional `corpus` parameter naming the corpus to search — `feeds`, `kb`, or `both` — and optional `source` and `path` parameters narrowing a knowledge search; an absent `corpus` SHALL behave exactly as `feeds`. Each citation SHALL carry a `kind` discriminator of `feed` or `doc`, a `snippet`, and a `feed_id`; a feed citation SHALL carry its feed identifier in `feed_id`, and a knowledge citation SHALL carry zero in `feed_id` together with its `doc_id`, `source_name`, `rel_path`, `heading_path`, and `locator`. The `citations` array SHALL always be a JSON array and never `null`.

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

## ADDED Requirements

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
