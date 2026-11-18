# Tasks

## 1. Validation and floors

- [x] 1.1 Raise `kbRecallMinLabelledQueries` to 55 and `kbRecallMinNegativeQueries` to 12, and add the `vague_referential`, `cluster_confusable`, `opaque_id`, and `symptom` category values to the validation switch. Verify: the existing 31/8 corpus fails validation with the new floors and names the missing categories.
- [x] 1.2 Require at least four labelled queries for each of the four new categories and at least six each for `semantic`, `paraphrase`, and `buried`. Verify: unit coverage pins a corpus that misses one category floor failing, and a corpus meeting every floor passing.
- [x] 1.3 Keep the duplicate relevant/exclude rejection and the existing label invariants unchanged. Verify: the duplicate-key unit test still passes and a corpus with a repeated relevant entry fails validation.

## 2. Corpus documents

- [x] 2.1 Add `B-Drink/tea.md` and `B-Drink/water.md` with 3–4 sections each, sharing beverage vocabulary without duplicating the coffee answers. Verify: the beverage directory holds at least three documents and each new heading yields exactly one chunk.
- [x] 2.2 Add `B-Sport/cycling.md`, `B-Sport/swimming.md`, and `B-Sport/injury.md` with 3–4 sections each, including knee/foot pain symptom sections. Verify: the sport directory holds at least five documents and the one-chunk-per-heading validation passes.
- [x] 2.3 Add `B-BodyCare/skin.md`, `B-Cook/baking.md`, and `A-Art/piano.md` with symptom sections, a recipe identifier, and piano work identifiers (`Op.27 No.2`, a Hanon exercise number). Verify: every target section exists as its own chunk and its identifier appears in exactly one chunk.
- [x] 2.4 Keep the total fixture at 24–28 documents and every added section inside the chunk budget. Verify: `kbRecallAllChunks` produces 85–95 chunks with no duplicate `(document, heading)` key.

## 3. Queries and labels

- [x] 3.1 Add the 29 planned labelled queries plus four bare-identifier probes to reach 64: semantic +2, paraphrase +2, same-topic distractor +1, multi-relevant +1, buried +4, vague-referential +5, cluster-confusable +5, opaque-id +8 (four contextual, four bare), symptom +5. Verify: fixture validation passes with every category above its floor and 64 labelled queries total.
- [x] 3.2 Add four near-miss negatives (protein powder/creatine, knee-brace selection, coffee-machine descaling, sleep-mask brands) to reach 12 negatives and mark them with `near_miss: true`. Verify: each answer is absent from the corpus, every negative has an empty relevant list, the near-miss count floor passes, and a `near_miss` marker on a labelled query fails validation.
- [x] 3.3 Confirm every path-scoped query still has more eligible chunks than `top_k` after the corpus growth, and every new label references an existing chunk key. Verify: candidate-set validation passes for all path queries and no relevant/exclude key is unknown.

## 4. Regeneration, measurement, and docs

- [x] 4.1 Regenerate `kb_recall_vectors.json` with `NYX_KB_RECALL_REGEN=1` against the local embedding server and run `TestKbRecallCorpusFixtureValid`. Verify: one vector per chunk and per query, every `text_sha256` matches, and fixture validation is green.
- [x] 4.2 Run the three scenarios against pgvector and record per-scenario and per-category `recall@1/@5/@10`, `hit@5`, mean `first_rank`, random control, negative top-1, decoy cosine range, and eligible row count. Verify: all three scenarios execute, the summary carries every metric, and the row counts remain 85+ / 2000+.
- [x] 4.3 Re-pin the `recall@5` floor at the worst scenario minus a 0.02–0.05 margin and the aggregate negative ceiling at the highest measured negative mean plus a margin. Verify: the suite passes at the pinned values, a temporarily raised floor fails with the scenario name and observed value, and the aggregate negative mean stays below the new ceiling.
- [x] 4.4 Update `docs/knowledge-base.md` with the expanded document/chunk counts, the full category table, the 60/12 split, the near-miss negatives, the measured table, and the re-pinned floor and ceiling. Verify: every documented command runs as written and the numbers match the test output.

## 5. Integration verification

- [x] 5.1 Run `SKIP_CONTAINERS=true go test ./internal/kb` and the full DB-backed recall baseline; both must exit 0 with no skipped retrieval scenario in the DB run. Verify: fixture validation runs without a database and the baseline reports all three scenarios.
- [x] 5.2 Run `DB_URL=... go test -race ./internal/kb -run 'TestKbRecall'` and `CI=true make coverage-check`. Verify: both exit 0 and the coverage gate's `SKIP_CONTAINERS=true` path still skips the recall scenarios.
- [x] 5.3 Run `golangci-lint run --timeout=5m`, `gofmt -l` on the changed Go files, `make sqlc-diff`, and `make openapi-diff`. Verify: zero findings and no drift.
- [x] 5.4 Confirm the diff touches only `backend/internal/kb/` test and testdata files plus `docs/knowledge-base.md`, with no production code, no `posts_douban`, and no decoy/config/measurement changes. Verify: `git diff --stat` and a grep for `posts_douban` in the changed tree.
