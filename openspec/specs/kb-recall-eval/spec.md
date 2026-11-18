# kb-recall-eval Specification

## Purpose

A deterministic, CI-runnable recall baseline for the knowledge corpus that measures ranking quality under realistic competition instead of trivially satisfied candidate sets.

## Requirements

### Requirement: Checked-in deterministic corpus

The baseline SHALL source every document from Markdown files checked into `backend/internal/kb/testdata/kb_recall_docs/`, every label from the checked-in corpus file, and every vector from the checked-in frozen vector file. A default run MUST NOT read `posts_douban`, any path outside the package's `testdata` tree, or call a live embedding model. Chunk texts MUST continue to be produced by the production normalizer and chunker, and the frozen vectors MUST be keyed by the SHA-256 of those chunk texts so chunker drift fails fixture validation instead of silently changing the measured corpus.

#### Scenario: Default run needs no embedding server

- **WHEN** the baseline runs with `DB_URL` pointing at a pgvector Postgres and no embedding endpoint configured
- **THEN** indexing, retrieval, and every scenario complete using only the checked-in documents, labels, and frozen vectors, and no outbound embedding call is made

#### Scenario: Chunker drift fails instead of re-measuring

- **WHEN** the production chunker changes the embed text of a fixture document
- **THEN** fixture validation fails with a message naming the chunk whose text no longer matches its frozen hash, and the run does not fall back to computing a new vector

### Requirement: No vacuously satisfied labelled query

At fixture validation time, every labelled query whose recall is counted MUST have more eligible chunks inside its retrieval scope than the production result width `top_k`. For a path-scoped query the eligible set is the chunks under the query's path prefix; for an unscoped query it is the whole fixture corpus. A query whose eligible set is less than or equal to `top_k` MUST fail validation, because retrieval would return the entire candidate set and recall would be 1.0 independent of ranking.

#### Scenario: A directory smaller than top_k is rejected

- **WHEN** a path-scoped query filters to a directory holding at most `top_k` chunks
- **THEN** fixture validation fails and names the query, its directory, and the candidate count, so the corpus must grow or the query must move before the baseline can run

#### Scenario: A directory with more than top_k chunks is accepted

- **WHEN** every path-scoped query's directory holds more than `top_k` eligible chunks
- **THEN** validation passes and the query contributes a ranking-dependent recall value

### Requirement: Decoy scenarios exercise ANN pressure

The baseline SHALL run the same labelled queries under at least two decoy scenarios: one populated with noise vectors and one populated with near-neighbour vectors close to the fixture corpus, mirroring the feed baseline's `scaled_noise` and `scaled_near` scenarios. Decoys SHALL be written as real chunk rows owned by the fixture user and eligible for retrieval, SHALL be counted before measurement, and the near-neighbour decoys SHALL sit close enough to the corpus to compete for top ranks. The run MUST report the eligible chunk row count per scenario and MUST fail when a scaled scenario has fewer eligible rows than the pinned ANN floor, so a scenario cannot claim ANN pressure while retrieving over a tiny table.

#### Scenario: Scaled scenario with eligible decoys

- **WHEN** a decoy scenario seeds at least the pinned ANN floor of eligible chunk rows
- **THEN** the scenario reports its row count and recall metrics, and the decoy rows are visible to the same retrieval predicate the fixture chunks use

#### Scenario: Decoys above the ANN floor are required

- **WHEN** a scaled scenario ends up with fewer eligible chunk rows than the pinned ANN floor
- **THEN** the run fails instead of reporting a recall number measured over a table too small to exercise the index

### Requirement: Measurement width and reported metrics

The baseline SHALL retrieve `k = 10` results per query while the production result width remains `top_k = 5`, and SHALL compute and report, per query and in aggregate, `recall@1`, `recall@5`, `recall@10`, `hit@5`, and `first_rank`. The headline regression metric SHALL be `recall@5`; `recall@10` and `first_rank` SHALL distinguish a gold chunk that ranks 6–10 from one that is not retrieved at all.

#### Scenario: Gold at rank 6–10 is distinguished from a miss

- **WHEN** a query's first relevant chunk is returned at rank 7
- **THEN** it counts as a miss for `recall@5` and `hit@5`, counts as a hit for `recall@10`, and reports `first_rank = 7`

#### Scenario: Aggregate reports every metric

- **WHEN** a scenario completes
- **THEN** the summary reports aggregate `recall@1`, `recall@5`, `recall@10`, `hit@5`, mean `first_rank`, the random-vector control, and the eligible row count

### Requirement: Query difficulty and label invariants

The labelled query set SHALL cover path-scoped, heading-scoped, exact-term, paraphrase-only semantic, cross-directory same-topic distractor without a path filter, multi-relevant, buried, and negative categories. Corpus validation SHALL reject duplicate query keys or texts, unknown categories, a labelled query with no relevant chunk, a non-negative query whose relevant count exceeds `top_k`, a relevant chunk that does not exist in the fixture, a path prefix that is not a directory prefix, and a relevant/exclude overlap. Negative queries SHALL carry no relevant chunks and SHALL NOT contribute to recall.

#### Scenario: Missing category or malformed label fails validation

- **WHEN** the corpus omits a required category or a labelled query references a chunk absent from the fixture
- **THEN** fixture validation fails before any database work and names the offending query

#### Scenario: Negative query measured by ceiling

- **WHEN** negative queries are evaluated
- **THEN** their top-1 similarity scores are aggregated separately and asserted below the pinned negative ceiling, and they are excluded from every recall figure

### Requirement: Floor derived from the hardened baseline

The pinned `recall@5` floor SHALL be derived from a measured run of the hardened corpus and decoy scenarios, with the measured value and the chosen margin recorded in the change's design and in `docs/knowledge-base.md`. The pre-hardening `0.85` floor MUST NOT be carried over unchanged, and the floor MUST NOT be lowered without recording the measured baseline that justified the change. The run SHALL also assert that real queries outperform the fixed random-vector control in every scenario.

#### Scenario: Degraded retrieval breaches the floor

- **WHEN** retrieval quality falls below the pinned floor
- **THEN** the baseline fails with the observed value, the pinned floor, and the scenario name in the failure message

#### Scenario: Random control cannot match real queries

- **WHEN** the fixed random query vector is evaluated under any scenario
- **THEN** its aggregate recall is lower than the real-query recall and the comparison is reported

### Requirement: CI executes, local absence may skip

The baseline SHALL execute in the default backend CI job. When `DB_URL` is unset, or points at a Postgres without pgvector, a local run MAY skip; when `CI` is set, the same condition MUST fail the run rather than skip, so a misconfigured CI database cannot make the gate vacuous. An explicit `SKIP_CONTAINERS=true` SHALL be honored as a deliberate opt-out and MAY skip even when `CI` is set; the coverage recipe relies on this, and the fail-closed rule applies to the implicit cases (empty `DB_URL` or a database without pgvector).

#### Scenario: CI without pgvector fails

- **WHEN** `CI` is set and the configured database cannot satisfy the pgvector requirement
- **THEN** the baseline fails and reports the missing prerequisite instead of skipping

#### Scenario: Local run skips cleanly

- **WHEN** `DB_URL` is unset on a local machine
- **THEN** the retrieval scenarios skip while fixture validation still runs

#### Scenario: Explicit container opt-out skips under CI

- **WHEN** `CI` is set and `SKIP_CONTAINERS=true` is passed explicitly, as the coverage recipe does
- **THEN** the retrieval scenarios skip without failing the coverage gate
