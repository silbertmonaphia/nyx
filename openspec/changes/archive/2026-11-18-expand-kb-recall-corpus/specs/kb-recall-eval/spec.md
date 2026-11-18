# Spec Delta

## MODIFIED Requirements

### Requirement: Query difficulty and label invariants

The labelled query set SHALL cover path-scoped, heading-scoped, exact-term, paraphrase-only semantic, cross-directory same-topic distractor without a path filter, multi-relevant, buried, vague-referential, cluster-confusable, opaque-identifier, symptom, and negative categories. The set SHALL contain at least 55 labelled queries and at least 12 negative queries, of which at least four negatives SHALL target topics adjacent to the corpus rather than wholly unrelated topics. The vague-referential, cluster-confusable, opaque-identifier, and symptom categories SHALL each carry at least four labelled queries, and the semantic, paraphrase, and buried categories SHALL each carry at least six. Corpus validation SHALL reject duplicate query keys or texts, duplicate relevant or exclude entries inside one query, unknown categories, a labelled query with no relevant chunk, a non-negative query whose relevant count exceeds `top_k`, a relevant chunk that does not exist in the fixture, a path prefix that is not a directory prefix, and a relevant/exclude overlap. Negative queries SHALL carry no relevant chunks and SHALL NOT contribute to recall.

#### Scenario: Missing category or malformed label fails validation

- **WHEN** the corpus omits a required category, falls below the labelled or negative query floor, or a labelled query references a chunk absent from the fixture
- **THEN** fixture validation fails before any database work and names the offending query or the missing category and count

#### Scenario: Negative query measured by ceiling

- **WHEN** negative queries are evaluated
- **THEN** their top-1 similarity scores are aggregated separately and asserted below the pinned negative ceiling, and they are excluded from every recall figure

#### Scenario: Near-miss negatives feed the aggregate ceiling

- **WHEN** a negative query targets a topic adjacent to the corpus
- **THEN** it carries no relevant chunks, contributes no recall, and its top-1 score is included in the aggregate negative-ceiling assertion

#### Scenario: Duplicate relevant entry is rejected

- **WHEN** one query lists the same chunk twice in its relevant or exclude list
- **THEN** fixture validation fails instead of silently dividing that query's recall by the duplicate count
