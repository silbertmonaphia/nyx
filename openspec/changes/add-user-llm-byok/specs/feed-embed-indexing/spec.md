# Spec Delta

## Purpose

Feed embedding-indexing becomes per-user BYOK: the indexer resolves the feed owner's embedder; an unconfigured owner's writes still succeed (skip, warn, stay healable), and configuring later is picked up by the next backfill.

## MODIFIED Requirements

### Requirement: Write-path embedding is best-effort within a bounded wait

Feed create, update, and delete SHALL attempt embedding-index maintenance with a deadline no greater than the configured `FEED_INDEX_TIMEOUT` (default 5 seconds). The embedder SHALL be resolved per feed owner from the owner's stored LLM configuration; when the owner has no configuration, the attempt SHALL be skipped entirely (no embedder exists to call) and reported to the write path as a skipped-attempt error, so the write still succeeds and the write path's existing best-effort handling drops any stale embedding row for that feed. On deadline, indexer error, or unconfigured owner, the write SHALL succeed and emit a warning identifying the feed; indexing failure never fails or delays a write beyond the cap. The indexer attempt SHALL still run when the owner's embedder is configured and healthy.

#### Scenario: Unreachable embedder does not hold the write

- **WHEN** a feed write runs while the owner's embedding server hangs and the attempt exceeds the cap
- **THEN** the write returns success within the cap, the content is stored, and a warning names the feed

#### Scenario: Healthy embedder indexes within the cap

- **WHEN** a feed write runs against the owner's configured, healthy embedder
- **THEN** the embedding row is written before the call returns, as today

#### Scenario: Unconfigured owner skips indexing, write succeeds

- **WHEN** a feed write runs for a user with no stored LLM configuration
- **THEN** no embedding endpoint is contacted, the write returns success, and a warning names the feed

#### Scenario: Indexing uses the owner's embedding endpoint

- **WHEN** a feed write runs for a user with a stored embedding configuration
- **THEN** the embedding request is sent to that user's embedding endpoint with that user's embedding model, not to the deployment's configured endpoint

#### Scenario: Delete is capped the same way

- **WHEN** a feed delete runs while the owner's embedding server hangs
- **THEN** the delete returns success within the cap; the stale embedding row, if any, is harmless to the retriever

### Requirement: A skipped index stays healable

A write whose indexing was skipped by the cap — or because the owner had no configuration — SHALL leave the feed eligible for the retrieve-time backfill. Eligibility is a missing embedding row — the backfill selects only feeds without one — so a skipped create/update SHALL best-effort delete the feed's embedding row (same cap) rather than leave stale content indexed. The next `BackfillOnce` for that user SHALL index the feed once an embedder is resolvable, and a `BackfillOnce` run that finds the user unconfigured MUST NOT mark itself as having run, so a later configuration is honored on the next invocation without a process restart.

#### Scenario: Backfill repairs a capped write

- **WHEN** a write was skipped during an embedder outage and the embedder recovers
- **THEN** a later RAG chat's backfill writes the missing embedding and the feed is retrievable

#### Scenario: A skipped update drops the stale row

- **WHEN** an update's indexing is skipped while a previous embedding row exists
- **THEN** that row is deleted (best-effort) so the feed is no longer retrievable with outdated content until the backfill re-embeds it

#### Scenario: Configuring later unblocks the backfill

- **WHEN** an unconfigured user's `BackfillOnce` runs (finding nothing to embed), the user then saves a configuration, and a later `BackfillOnce` runs in the same process
- **THEN** the later run embeds the user's feeds — the earlier run did not mark the user as done
