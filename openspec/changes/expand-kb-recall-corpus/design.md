# Design

## Context

See `proposal.md` — Why. Before this change, the hardened baseline (change `harden-kb-recall-baseline`, archived) had 18 documents, 63 chunks, 31 labelled queries, and 8 negatives. Its three scenarios (`corpus_only`, `scaled_noise`, `scaled_near`), 2000 decoys, 1000-row ANN floor, `k = 10` measurement width, and aggregate metrics are settled and stay unchanged. The category switch, count floors, and duplicate-key validation live in `backend/internal/kb/recall_integration_test.go`; category values live in `backend/internal/kb/testdata/kb_recall_corpus.json`; the then-current 63 chunk vectors and 39 query vectors lived in `kb_recall_vectors.json`.

Feed's composition is the reference for difficulty coverage: 75 feeds, 68 queries split across symptom, semantic, buried, vague-referential, cluster-confusable, exact-keyword, multi-relevant, opaque-id, same-topic-distractor, and negative. KB currently lacks vague-referential, cluster-confusable, opaque-id, and symptom, and has too few semantic, paraphrase, and buried cases.

## Goals / Non-Goals

**Goals:**

- Reach 60 labelled queries and 12 negatives while keeping every existing category.
- Cover the four missing difficulty classes with real corpus support, not just labels.
- Make the `recall@5` metric granular enough that one boundary miss does not dominate the floor signal.
- Keep the run deterministic, model-free at measurement time, and CI-runnable.

**Non-Goals:**

- Reaching feed's exact query count or copying its query texts.
- Adding `posts_douban` or any real user notes.
- Changing decoys, ANN floor, scenario shape, measurement width, or aggregation.
- Adding a lexical leg, reranker, or fusion-quality measurement.
- Changing production retrieval, chunking, indexing, or the bridge.

## Decisions

### D1 — Add eight targeted documents, not a bulk corpus dump

The existing 18 documents already support path/heading/exact coverage. Add documents only where a new category needs a real gold chunk and a real distractor:

| Area | Added documents | Purpose |
|---|---|---|
| Beverages | `B-Drink/tea.md`, `B-Drink/water.md` | cluster-confusable with `coffee.md`; three beverage docs compete |
| Sport | `B-Sport/cycling.md`, `B-Sport/swimming.md`, `B-Sport/injury.md` | cluster-confusable with `running.md` / `gym.md`; symptom queries for knee/foot pain |
| Body care | `B-BodyCare/skin.md` | symptom queries (dryness, redness, itching) |
| Cooking | `B-Cook/baking.md` | symptom queries (dough not rising, collapsed cake) and an opaque recipe identifier |
| Art | `A-Art/piano.md` | opaque identifiers (`Op.27 No.2`, Hanon exercise number) and cluster with `music.md` |

Target: 26 documents, roughly 85–95 chunks. Each added section stays inside the chunk budget so one heading path keeps one chunk.

### D2 — Query distribution targets 60 labelled + 12 negative

| Category | Current | Target |
|---|---|---|
| path_scope | 5 | 5 |
| heading_scope | 6 | 6 |
| exact_term | 4 | 4 |
| semantic | 4 | 6 |
| paraphrase | 4 | 6 |
| same_topic_distractor | 2 | 3 |
| multi_relevant | 4 | 5 |
| buried | 2 | 6 |
| vague_referential | 0 | 5 |
| cluster_confusable | 0 | 5 |
| opaque_id | 0 | 4 |
| symptom | 0 | 5 |
| **labelled total** | **31** | **60** |
| negative | 8 | 12 |

The four new classes get concrete query shapes:

- `vague_referential`: "上次说的那个煮饭的方子", "之前那篇讲睡眠的", "有个说配速的" — no topic nouns, the label resolves the referent.
- `cluster_confusable`: one topic spread across several documents (coffee/tea/water, running/cycling/swimming, music/piano); the query targets one document while its sibling documents are valid near-misses.
- `opaque_id`: rare identifiers (`Op.27 No.2`, a recipe code, a Hanon number) that only one chunk contains.
- `symptom`: "面团发不起来", "膝盖外侧疼", "皮肤又干又痒" — a described symptom that must map to a troubleshooting section.

Near-miss negatives (4) target adjacent but not answered topics: protein powder/creatine, knee-brace selection, coffee-machine descaling, sleep-mask brands. The answers stay absent from the corpus while adjacent vocabulary remains (蛋白, 眼罩, 膝盖, 咖啡), which is what makes them near-miss; running-shoe replacement was rejected because the existing `B-Sport/running.md` already answers it. Each near-miss query carries `near_miss: true`, and validation requires at least four such markers and rejects one on a labelled query.

### D3 — Validation encodes the new floors

`kbRecallMinLabelledQueries` rises to 55 (target 60) and `kbRecallMinNegativeQueries` to 12. The category switch adds `vague_referential`, `cluster_confusable`, `opaque_id`, and `symptom`. Validation requires each of those four to carry at least four labelled queries and requires `semantic`, `paraphrase`, and `buried` to carry at least six each. The duplicate relevant/exclude rejection and all existing label invariants stay unchanged.

### D4 — Floor and ceiling are re-derived, not carried over

After regeneration and a full three-scenario run, record the per-scenario and per-category table. Pin `recall@5` at the worst scenario's measured value minus a 0.02–0.05 margin, and the negative top-1 ceiling at the highest measured negative mean plus a margin. Near-miss negatives will score higher than the current far-topic negatives, so the ceiling must be measured with them present. The old `0.94` floor and `0.40` ceiling are not assumed.

### D5 — Vectors are regenerated, not hand-edited

Run `TestRegenerateKbRecallVectors` against the local embedding server after the document and query edits. The regenerated file must contain one vector per chunk and one per query; `TestKbRecallCorpusFixtureValid` must pass with matching `text_sha256` values. The fixture stays under `backend/internal/kb/testdata/`.

### D6 — Docs record the expanded corpus and new numbers

Update the recall-baseline section in `docs/knowledge-base.md`: document/chunk counts, the full category table, the 60/12 split, the near-miss negatives, the measured per-scenario table, and the re-pinned floor and ceiling.

## Risks / Trade-offs

- **Harder queries lower the measured `recall@5`** → expected; the floor is re-derived from the new measurement rather than treated as a regression. Record both the old and new numbers.
- **Near-miss negatives breach the old ceiling** → re-derive the ceiling after measurement; keep negatives excluded from recall.
- **Corpus growth makes one heading produce multiple chunks** → keep every added section inside the chunk budget; the one-chunk-per-heading validation fails loudly otherwise.
- **Vectors file roughly doubles** → acceptable; regen is seconds and CI runtime is dominated by decoy seeding, which is unchanged.
- **New labels can be ambiguous** → each new query names a single gold chunk (or a small multi-relevant set), and validation rejects missing/unknown/duplicate labels.
- **Cluster-confusable documents may rank too easily** → measure by category; if a cluster collapses to one document, add sibling sections that share vocabulary but not the answer.

## Measured Baseline

Expanded corpus: 26 documents, 89 chunks, 60 labelled queries, 12 negatives (4 near-miss). Frozen vectors from `Qwen/Qwen3-Embedding-0.6B`; decoys unchanged at 2000 per scaled scenario.

| Scenario | Recall@1 | Recall@5 | Recall@10 | Hit@5 | Mean first rank | Random@5 | Negative top-1 | Rows | Distinct top-1 |
|---|---|---|---|---|---|---|---|---|---|
| `corpus_only` | 0.825 | 0.992 | 1.000 | 1.000 | 1.183 | 0.108 | 0.426 | 89 | 46/60 |
| `scaled_noise` | 0.825 | 0.992 | 1.000 | 1.000 | 1.183 | 0.000 | 0.426 | 2089 | 46/60 |
| `scaled_near` | 0.825 | 0.983 | 1.000 | 1.000 | 1.183 | 0.000 | 0.426 | 2089 | 46/60 |

Noise-decoy cosine range `[-0.111, 0.101]`; near-decoy range `[0.350, 0.749]`. Per-category `recall@5` is 1.000 except multi-relevant: 0.900 in `corpus_only`/`scaled_noise` and 0.800 in `scaled_near`; the misses are `q-multi-mealprep` (all scenarios) and `q-multi-insurance` (`scaled_near`), all recovered by `recall@10`.

Pinned floor: **0.95** = worst scenario `0.983` minus a `0.033` margin. Negative top-1 ceiling: **0.48** = measured aggregate `0.426` with near-miss negatives plus a `0.054` margin. The ceiling is aggregate-only by design: three of the four near-miss negatives individually exceed `0.48` while the 12-negative mean stays below it. The near-miss negatives raised the mean from the previous `0.348`, which is why the ceiling moves with them.

## Migration Plan

Test-only change: regenerate `kb_recall_vectors.json`, run fixture validation and the three scenarios locally, then commit the corpus, vectors, test changes, and docs together. No migrations, no deploy step, no rollout gates; CI runs the baseline through the existing `backend-test` job.
