# Spec Delta

## Purpose

RAG bridge retrieval under strict BYOK: a user without a stored LLM configuration has no embedder to resolve, and the bridge degrades to the same 200-empty envelope as "no matches" — never an error, never a cross-user leak, and no change to the frozen Python-side wire contract.

## ADDED Requirements

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
