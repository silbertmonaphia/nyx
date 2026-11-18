# Spec Delta

## ADDED Requirements

### Requirement: Rerank quality is measured opt-in without changing the default run

The default baseline SHALL continue to measure the dense leg only and SHALL NOT call a live reranker. An opt-in rerank measurement SHALL reuse the same checked-in documents, labels, and frozen query vectors, run the retrieved candidates through the production rerank stage using a test-configured reranker endpoint, and report the same `recall@1`, `recall@5`, `recall@10`, `hit@5`, and `first_rank` family per scenario and in aggregate. The measurement SHALL be skipped unless a reranker endpoint is supplied for the test run, and its measured numbers and the enabling command SHALL be recorded in `docs/knowledge-base.md`. The opt-in measurement SHALL NOT replace, weaken, or re-derive the dense floors, and the test-only endpoint setting MUST NOT become a runtime fallback for user requests.

#### Scenario: Default run stays dense-only

- **WHEN** the baseline runs without a reranker endpoint supplied
- **THEN** no reranker call is made and the existing dense floors apply unchanged

#### Scenario: Opt-in measurement uses the same corpus and metrics

- **WHEN** a reranker endpoint is supplied and the rerank measurement is enabled
- **THEN** each scenario reports the same metric family with the rerank stage applied, and the measured table is recorded in the knowledge-base guide

#### Scenario: Opt-in absence skips cleanly

- **WHEN** the dataset is available but no reranker endpoint is supplied and the measurement was requested
- **THEN** the rerank measurement skips without failing the dense baseline
