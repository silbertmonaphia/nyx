# Spec Delta

## MODIFIED Requirements

### Requirement: The feed corpus is unaffected

A `feeds` retrieval SHALL return the same passages the feed corpus returned before this change; its citations gain the additive `kind` discriminator required by the bridge delta. Its order SHALL match the pre-existing feed retrieval when the caller has no rerank group configured; when the caller has a rerank group, the order SHALL be the feed corpus's reranked order.

#### Scenario: Feeds-only retrieval is unchanged

- **WHEN** a retrieval names the `feeds` corpus and the caller has no rerank group configured
- **THEN** the returned passages and their order match the pre-existing feed retrieval for the same query

#### Scenario: Feeds-only retrieval follows the reranker when configured

- **WHEN** a retrieval names the `feeds` corpus and the caller has a rerank group configured
- **THEN** the returned passages are the same set the feed corpus selects, ordered by the reranker

### Requirement: Both-corpus fusion is by rank

A `both` retrieval SHALL take the top results from each corpus and combine them by rank rather than by score, so a result's position within its own corpus — not its raw similarity number — decides the combined order. Each corpus's contribution SHALL be its post-rerank order when the caller has a rerank group configured.

#### Scenario: Both-corpus retrieval fuses by rank

- **WHEN** a retrieval names the `both` corpus and the two corpora return passages with different similarity scores
- **THEN** the combined order follows each passage's rank within its own corpus rather than its raw score

#### Scenario: Fusion consumes post-rerank ranks

- **WHEN** a retrieval names the `both` corpus and the caller has a rerank group configured and the reranker reorders one corpus
- **THEN** the combined order follows the post-rerank ranks and no raw reranker score is compared across corpora
