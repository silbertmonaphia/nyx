# Proposal

## Why

The KB recall baseline reports `recall@5 = 1.000`, but that number is not evidence of retrieval quality. Three of the five path-scoped queries filter into a directory holding fewer than `top_k = 5` chunks, so the retrieval returns the whole candidate set and recall is 1.0 regardless of ranking. The corpus also has only 43 chunks, no decoys, and reports no `recall@1` or `recall@10`, so it cannot be compared with the feed baseline (62 labelled queries, `recall@5 = 0.944` under 2000 near-neighbour decoys). The baseline is a useful regression gate but a misleading quality claim; harden it before anyone cites the number.

## What Changes

- Remove the three vacuous path-scoped queries (`q-caffeine-sleep`, `q-teeth-routine`, `q-travel-prep`) by making every path-scoped query's filtered candidate set larger than `top_k` — either by adding documents under the filtered directory or by relocating the query — and enforce that invariant in fixture validation.
- Add decoy scenarios mirroring `internal/rag`: noise decoys and near-neighbour decoys inserted as real chunk rows, so the baseline measures recall under ANN pressure instead of a 43-row exact scan, and record whether the decoy rows are actually present and eligible for retrieval.
- Align the measurement with the feed baseline: retrieve `k = 10` from the store while the production result set remains `top_k = 5`, then report `recall@1`, `recall@5`, `recall@10`, `hit@5`, and `first_rank` instead of only `recall@5`.
- Extend the labelled query set with harder categories: paraphrase-only semantic queries, cross-directory same-topic distractors without a path filter, multi-relevant queries, buried queries whose gold chunk ranks 6–10, and negative queries with a pinned top-1 score ceiling.
- Re-derive the recall floor from a measured run of the hardened corpus; the existing `0.85` floor is not carried over. Record the measured baseline and the floor derivation in the design and in `docs/knowledge-base.md`.
- Update `docs/knowledge-base.md`'s recall-baseline section to describe the hardened corpus, the decoy scenarios, the measurement width, and the new floor.
- Keep the corpus synthetic, deterministic, checked in, and CI-runnable. Do not commit `posts_douban` or any real user notes. No production retrieval code changes.

## Capabilities

### New Capabilities

- `kb-recall-eval`: the deterministic KB recall baseline — its corpus and label invariants, decoy scenarios, measurement width and metrics, floor derivation, and CI execution contract.

### Modified Capabilities

None. Production retrieval, chunking, indexing, and the bridge are unchanged; this is test and evaluation tooling only.

## Impact

- `backend/internal/kb/recall_integration_test.go` — scenario loop, decoy seeding, metric computation, fixture validation, floor assertions.
- `backend/internal/kb/testdata/kb_recall_corpus.json` — query categories and labels; candidate-set invariants.
- `backend/internal/kb/testdata/kb_recall_docs/` — added documents and sections to remove vacuous path scopes and host harder queries.
- `backend/internal/kb/testdata/kb_recall_vectors.json` — regenerated through the production chunk/embed path after the corpus and query changes.
- `docs/knowledge-base.md` — recall-baseline section.
- CI: the existing `backend-test` job already runs the suite against `pgvector/pgvector:pg17` with `DB_URL`; the hardened baseline stays in that job.
- No rollout gates: test-only change by the rollout-split classification test, so no `<change>-rollout` sibling.
