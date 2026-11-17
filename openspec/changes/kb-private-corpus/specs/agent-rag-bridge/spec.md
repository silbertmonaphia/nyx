# Spec Delta

## MODIFIED Requirements

### Requirement: The knowledge corpus is retrieved through the same bridge and the same funnel

A knowledge search SHALL be performed by the Go backend on behalf of the authenticated caller, SHALL scope its results to that caller, and SHALL be subject to the same safe-error funnel as a feed search: an internal failure SHALL be logged with the request identifier and SHALL NOT surface an internal error string in the response body.

#### Scenario: Knowledge retrieval failure yields the empty envelope

- **WHEN** the knowledge search fails internally
- **THEN** the endpoint returns the standard empty envelope and the internal error is logged with the request identifier rather than returned

#### Scenario: Knowledge retrieval is scoped to the caller

- **WHEN** a knowledge search runs on behalf of an authenticated caller
- **THEN** it searches only the caller's own sources

### Requirement: The bridge exposes a knowledge outline for the caller's visible corpus

The bridge SHALL expose a read-only endpoint that returns, for each source visible to the authenticated caller, the source identifier, display name, document count, chunk count, and top-level directory names. The response SHALL contain no document content and no absolute filesystem path, and SHALL list no source that is not visible to the caller.

#### Scenario: Outline lists the visible sources

- **WHEN** the outline endpoint is called with a valid Bearer token
- **THEN** it lists the caller's own sources with their counts and top-level directory names, and no other user's source

#### Scenario: Outline is not advertised publicly

- **WHEN** a client fetches `/api/swagger/doc.json`
- **THEN** neither the outline path nor the retrieval path appears in the OpenAPI document
