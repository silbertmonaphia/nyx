# Proposal

## Why

The hardened KB recall baseline measures `recall@5 = 0.984` with only one miss, but its query set is still thin: 31 labelled queries, 4 semantic, 2 buried, 2 same-topic distractors, and no `vague_referential`, `cluster_confusable`, `opaque_id`, or `symptom` cases. Feed's baseline covers those classes with 62 labelled queries, which is why its misses cluster in semantic paraphrase and multi-relevant crowding instead of disappearing. At 31 labelled queries a single single-gold miss moves the mean by `1/31 = 0.032`, so the `0.94` floor tolerates only one additional miss before turning red. The KB number is a regression gate, not a difficulty measure; expanding the corpus and query classes makes it comparable to feed's coverage and gives the floor a stable signal.

## What Changes

- Grow labelled queries from 31 to 55–60 and negative queries from 8 to 12, including four near-miss negatives whose topics are adjacent to the corpus (for example supplements and running shoes) rather than wholly unrelated.
- Add four required categories previously absent from the KB corpus: `vague_referential`, `cluster_confusable`, `opaque_id`, and `symptom`. Expand `semantic`, `paraphrase`, and `buried` by 4–6 labelled queries each. Keep every existing category.
- Grow the fixture corpus from 18 to 24–28 documents to support the new queries, including same-topic multi-document clusters and long documents with several similar sections.
- Extend fixture validation to require the expanded category set and the new labelled/negative floors, while keeping the existing label invariants (unique keys/texts, no empty relevant sets, relevant count ≤ `top_k`, path prefix shape, no relevant/exclude overlap, duplicate-key rejection).
- Keep the decoy machinery unchanged: 2000 noise/near decoys per scaled scenario, the 1000-row ANN floor, three scenarios, `k = 10` measurement width, and the aggregate metrics.
- Re-measure the expanded corpus and re-pin the `recall@5` floor and negative top-1 ceiling from the observed values; update the measured table and category breakdown in `docs/knowledge-base.md`.
- Keep the corpus synthetic, deterministic, checked in, and CI-runnable. Do not read `posts_douban`; do not change production retrieval, chunking, indexing, config, or the bridge.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `kb-recall-eval`: the query-difficulty and label-invariant requirement now names the four added categories and raises the labelled/negative floors; the fixture may no longer be accepted with the old 31-query shape.

## Impact

- `backend/internal/kb/testdata/kb_recall_corpus.json` — new queries, labels, and category values.
- `backend/internal/kb/testdata/kb_recall_docs/` — 6–10 added documents and sections.
- `backend/internal/kb/testdata/kb_recall_vectors.json` — regenerated through the production chunk/embed path.
- `backend/internal/kb/recall_integration_test.go` — category validation and count floors; no measurement or decoy mechanics change.
- `docs/knowledge-base.md` — corpus shape, category table, measured baseline, pinned floor/ceiling.
- CI: still runs in the existing `backend-test` job against pgvector; decoy count and runtime stay unchanged.
- No rollout gates: test-only change by the rollout-split classification test, so no `<change>-rollout` sibling.
