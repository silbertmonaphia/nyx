# Spec Delta

## MODIFIED Requirements

### Requirement: Indexing runs on an explicit trigger and on a bounded lazy catch-up

Indexing SHALL run when the caller explicitly triggers it over the caller's own enabled sources. Indexing SHALL ALSO run as a background catch-up when a chat request observes documents that are still pending or chunks that are still unembedded. The catch-up SHALL run on a detached context, SHALL be bounded in the work it performs per invocation, and SHALL NOT delay or fail the chat request that triggered it.

#### Scenario: Explicit trigger indexes the caller's visible sources

- **WHEN** a user triggers indexing
- **THEN** the caller's own enabled sources are scanned and their changed documents are indexed

#### Scenario: Chat triggers a bounded catch-up

- **WHEN** a chat request observes a pending document
- **THEN** a background catch-up begins on a detached context and the chat request completes without waiting for it

#### Scenario: Catch-up does not fail the chat

- **WHEN** the catch-up fails
- **THEN** the chat request still succeeds and the failure is recorded on the source
