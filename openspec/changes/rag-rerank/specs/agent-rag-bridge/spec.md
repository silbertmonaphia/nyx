# Spec Delta

## MODIFIED Requirements

### Requirement: Endpoint exposes rag.Service.Retrieve

The service SHALL expose `GET /internal/rag/retrieve` with required `query` and `user_id` parameters, returning the rendered context envelope on success and the documented status codes on failure. It SHALL accept an optional corpus selector and knowledge scope parameters; citations SHALL carry a corpus discriminator and corpus-specific identity fields. The envelope and citation fields SHALL be unchanged by the rerank stage; only passage order and the rendered context block may differ.

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
- **THEN** the endpoint searches the feed corpus, returns the same citation shape it returned before the knowledge corpus existed, and returns the passages that corpus selects in its own order — its reranked order when the caller has a rerank group configured

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
