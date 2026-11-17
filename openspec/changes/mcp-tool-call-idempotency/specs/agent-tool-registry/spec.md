# Spec Delta

## ADDED Requirements

### Requirement: Mutating tool schemas declare an optional idempotency key

The mutating feed tools (`create_feed`, `update_feed`, `delete_feed`) SHALL declare an optional `idempotency_key` string argument in their served input schema, with a bounded length and a restricted character set. The argument SHALL be optional: a caller that omits it receives today's behavior with no deduplication. Read-only tools SHALL NOT declare the argument. The MCP `tools/list` surface SHALL expose the argument so external agents can opt in, and the tool descriptions SHALL document its semantics.

#### Scenario: Mutating tool advertises the optional key

- **WHEN** `tools/list` returns `create_feed`
- **THEN** its `inputSchema` includes an optional `idempotency_key` property, and omitting it is valid

#### Scenario: Read-only tool does not advertise a key

- **WHEN** `tools/list` returns `list_feeds` or `get_feed`
- **THEN** its `inputSchema` does not include `idempotency_key`

#### Scenario: Caller without a key keeps legacy behavior

- **WHEN** an external MCP agent calls `create_feed` without `idempotency_key`
- **THEN** the call dispatches and behaves exactly as before this change

### Requirement: Mutating dispatches are deduplicated per owner and key

When a mutating tool call carries an `idempotency_key`, the registry SHALL record the call and its response atomically with the underlying write: the idempotency record and the data mutation MUST commit in the same Postgres transaction, keyed by the authenticated owner id and the key. A dispatch whose (owner, key) already has a recorded response SHALL return that stored response without re-executing the write. The stored response MUST be the original successful tool result — including `delete_feed` returning its original success rather than a not-found error on replay. A concurrent duplicate SHALL wait for the first transaction and then return the stored response. Keys MUST be validated for length and character set before the tool body runs; an invalid key MUST return a typed validation error. Deduplication MUST NOT allow one owner to read another owner's stored response.

#### Scenario: Duplicate create returns the original row

- **WHEN** `create_feed` is dispatched twice with the same owner and key and identical arguments
- **THEN** exactly one feed row exists and both calls return the same feed object

#### Scenario: Replayed delete returns success

- **WHEN** `delete_feed` completes successfully and the same owner and key are dispatched again
- **THEN** the second dispatch returns the original success response and does not surface a not-found error

#### Scenario: Concurrent duplicates execute once

- **WHEN** two dispatches with the same owner and key race
- **THEN** one mutation commits, the other returns the same stored response, and no duplicate row is created

#### Scenario: Cross-owner keys are isolated

- **WHEN** two owners present the same key value
- **THEN** each dispatch executes independently and neither can read the other's stored response

#### Scenario: Invalid key fails before the tool body

- **WHEN** a caller supplies an `idempotency_key` that violates the declared length or character set
- **THEN** the registry returns a typed validation error and the tool body does not run

#### Scenario: Missing key disables deduplication

- **WHEN** a mutating call omits `idempotency_key`
- **THEN** the mutation executes with today's semantics and no idempotency record is written

### Requirement: Idempotency records are retention-bounded

The stored idempotency records SHALL be purged after a bounded retention window. Purging MUST only end the deduplication window; it MUST NOT modify tool data or leave a partially applied mutation.

#### Scenario: Expired records are purged

- **WHEN** the retention cutoff passes for an idempotency record
- **THEN** the record is deleted and the underlying feed data is unchanged
