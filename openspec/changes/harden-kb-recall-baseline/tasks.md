# Tasks

## 1. Harness measurement plumbing

- [x] 1.1 Keep the production width `kbRecallTopK = 5` for scoring, add a measurement width `kbRecallFetchK = 10`, and pass `Limit: kbRecallFetchK` to `RetrievePassages`. Verify: the retrieval assertions accept up to 10 passages while every recall offset is computed from the first 5 or the first 10 of that list.
- [x] 1.2 Replace the single `recall@5` computation with per-query `recall@1`, `recall@5`, `recall@10`, `hit@5`, and `first_rank`, and aggregate each over labelled queries and per category. Verify: a table-driven unit test with a synthetic retrieved/relevant list pins every metric, including a gold chunk at rank 7 counting for `recall@10` but not `recall@5` or `hit@5`.
- [x] 1.3 Add candidate-set validation: for every labelled query, compute the eligible chunk count from `kbRecallAllChunks` (path prefix when the query has one, whole fixture otherwise) and fail when the count is `<= kbRecallTopK`, naming the query, directory, and count. Verify: a unit test with a synthetic fixture rejects a 5-chunk directory and accepts a 6-chunk directory; the current fixture fails validation on the three vacuous queries.
- [x] 1.4 Make the pgvector prerequisite fail closed in CI: extract a pure decision helper from `requireKbRecallPgvector` that returns skip for local runs and for an explicit `SKIP_CONTAINERS=true` opt-out, but fatal when `CI` is set and `DB_URL`/pgvector is missing without that opt-out. Verify: a unit test pins the decision table including the coverage recipe's `CI=true` + `SKIP_CONTAINERS=true` combination.
- [x] 1.5 Keep the random-vector control and distinct-top-1 assertions in the new metric loop, reporting them per scenario. Verify: the control recall and distinct top-1 count appear in every scenario's summary and are asserted.

## 2. Corpus and labels

- [x] 2.1 Add documents and/or sections to `B-Sleep/`, `B-BodyCare/`, and `P-Travel/` so each directory holds more than `kbRecallTopK` chunks after the production chunker runs. Verify: candidate-set validation passes for `q-caffeine-sleep`, `q-teeth-routine`, and `q-travel-prep`, and the recorded per-directory chunk counts exceed 5.
- [x] 2.2 Add labelled queries for paraphrase-only semantic, cross-directory same-topic distractor without a path filter, multi-relevant, and buried categories, retaining all existing categories; reach at least 30 labelled queries and at least 8 negatives. Verify: fixture validation accepts the corpus, every required category is present, and no query is rejected for duplicate key/text, unknown category, empty relevant set, over-`top_k` relevant count, or path/relevant/exclude violations.
- [x] 2.3 Keep every added section within the chunk budget so one heading path still yields exactly one chunk. Verify: the one-chunk-per-heading validation passes and the fixture chunk count matches the number of frozen chunk vectors after regeneration.
- [x] 2.4 Regenerate `kb_recall_vectors.json` through `TestRegenerateKbRecallVectors` with `NYX_KB_RECALL_REGEN=1` against the local embedding server. Verify: `TestKbRecallCorpusFixtureValid` passes, every chunk's `text_sha256` matches, and the committed fixture has one vector per chunk and per query.

## 3. Decoy scenarios

- [x] 3.1 Add a decoy source for the fixture user (`UpsertDirSource` with a temp root) and batched raw-SQL seeding of decoy documents and chunks with owner, generation 1, `embedding_model = vectors.Model`, enabled source, synthetic content/heading/locator/hash, and a direct `vector(1024)` embedding. Verify: the seeded row count matches the requested decoy count and the rows join through the same retrieval predicate as the fixture chunks.
- [x] 3.2 Implement deterministic `noise` and `near` decoy vector builders with the existing seed: noise as normalized Gaussian vectors, near as a perturbed fixture vector normalized to a target cosine in `[0.35, 0.75]`. Verify: a unit test pins reproducibility (same seed → identical vectors) and the observed cosine range stays inside the target band.
- [x] 3.3 Wire three scenarios: `corpus_only` with the fixture source filter, `scaled_noise` and `scaled_near` with owner-only retrieval so decoys compete; default 2000 decoys per scaled scenario, `NYX_KB_RECALL_DECOYS` override, and a pinned ANN floor of 1000 eligible rows. Verify: each scaled scenario reports its eligible row count and fails when the count is below the ANN floor.
- [x] 3.4 Distribute decoy relative paths under the fixture's top-level directories so path-scoped queries face decoy competition inside their prefix. Verify: a path-scoped query in a scaled scenario sees more eligible chunks than the fixture directory alone, and still returns only paths under its prefix.
- [x] 3.5 Delete the decoy source in cleanup so documents and chunks cascade. Verify: after the run, no decoy source, document, or chunk row remains for the fixture user.

## 4. Baseline, floor, and docs

- [x] 4.1 Run the hardened baseline against a local pgvector Postgres and record per-scenario and per-category `recall@1/@5/@10`, `hit@5`, random control, negative top-1, decoy cosine range, and eligible row count. Verify: the measured table is committed in `docs/knowledge-base.md` and matches the test's logged output.
- [x] 4.2 Re-derive the negative top-1 ceiling under the decoy scenarios and pin it; negative queries stay excluded from recall. Verify: every scenario's negative top-1 mean is below the pinned ceiling and a corpus with a labelled negative query fails validation.
- [x] 4.3 Pin the `recall@5` floor at the worst scenario's measured value minus a 0.02–0.05 margin, replacing `0.85`; assert every scenario against it. Verify: the suite passes at the pinned floor, fails when the floor is locally raised above the measured value, and the failure message names the observed value, the floor, and the scenario.
- [x] 4.4 Update `docs/knowledge-base.md`'s recall-baseline section: corpus shape and categories, decoy scenarios, measurement width (`k = 10`, production `top_k = 5`), metric table, pinned floor, CI behavior, and regeneration commands. Verify: every command in the documented section runs as written.

## 5. Integration verification

- [x] 5.1 Run `cd backend && SKIP_CONTAINERS=true go test ./internal/kb` and confirm fixture validation runs green while the DB scenarios skip. Verify: exit 0 and the skip reason names the pgvector prerequisite.
- [x] 5.2 Run the full recall baseline with `DB_URL` pointing at a local pgvector Postgres and all three scenarios green. Verify: exit 0, no scenario skipped, and the summary reports every metric per scenario.
- [x] 5.3 Run the CI-shaped gate (`CI=true` plus the pgvector `DB_URL`) and confirm it fails closed when the prerequisite is missing and runs all scenarios when present; also run `CI=true make coverage-check` and confirm its `SKIP_CONTAINERS=true` opt-out skips the recall scenarios without failing the coverage gate. Verify: every invocation exits as specified, with no silent skip in the CI-shaped full run and no red coverage gate.
- [x] 5.4 Run `golangci-lint run --timeout=5m` and `gofmt -l` over the changed Go files. Verify: zero findings and no formatting diffs.
- [x] 5.5 Confirm the diff touches only `backend/internal/kb/` test and testdata files and `docs/knowledge-base.md`, with no production retrieval, chunking, indexing, config, or bridge changes, and no `posts_douban` reference. Verify: `git diff --stat` and a grep for `posts_douban` in the changed tree.
