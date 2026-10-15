# Spec Delta

## ADDED Requirements

### Requirement: Write-path embedding is best-effort within a bounded wait

Feed create, update, and delete SHALL attempt embedding-index maintenance with a deadline no greater than the configured `FEED_INDEX_TIMEOUT` (default 5 seconds). On deadline or indexer error the write SHALL succeed, emit a warning identifying the feed, and preserve the stored content — indexing failure never fails or delays a write beyond the cap. The indexer attempt itself SHALL still run when the embedder is healthy.

#### Scenario: Unreachable embedder does not hold the write

- **WHEN** a feed write runs while the embedding server hangs and the attempt exceeds the cap
- **THEN** the write returns success within the cap, the content is stored, and a warning names the feed

#### Scenario: Healthy embedder indexes within the cap

- **WHEN** a feed write runs against a healthy embedder
- **THEN** the embedding row is written before the call returns, as today

#### Scenario: Delete is capped the same way

- **WHEN** a feed delete runs while the embedding server hangs
- **THEN** the delete returns success within the cap; the stale embedding row, if any, is harmless to the retriever

### Requirement: The cap is configured, validated, and independent

`FEED_INDEX_TIMEOUT` SHALL default to 5 seconds and SHALL be rejected at startup when it is not a positive duration. It SHALL be independent of `LLM_TIMEOUT` — changing one SHALL NOT alter the other's behavior.

#### Scenario: Non-positive value is refused at boot

- **WHEN** the service starts with `FEED_INDEX_TIMEOUT=0s`
- **THEN** configuration validation fails with an error naming the variable

### Requirement: A skipped index stays healable

A write whose indexing was skipped by the cap SHALL leave the feed eligible for the retrieve-time backfill. Eligibility is a missing embedding row — the backfill selects only feeds without one — so a skipped create/update SHALL best-effort delete the feed's embedding row (same cap) rather than leave stale content indexed. The next `BackfillOnce` for that user SHALL index the feed.

#### Scenario: Backfill repairs a capped write

- **WHEN** a write was skipped during an embedder outage and the embedder recovers
- **THEN** a later RAG chat's backfill writes the missing embedding and the feed is retrievable

#### Scenario: A skipped update drops the stale row

- **WHEN** an update's indexing is skipped while a previous embedding row exists
- **THEN** that row is deleted (best-effort) so the feed is no longer retrievable with outdated content until the backfill re-embeds it
