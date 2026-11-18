# Design

## Context

See `proposal.md` — Why. The current baseline in `backend/internal/kb/recall_integration_test.go` indexes one directory source owned by a fresh user through the production chunker and indexer, retrieves `LIMIT 5` per labelled query, and asserts a mean `recall@5` floor of `0.85`. The fixture holds 12 documents, 43 chunks, and 23 queries (19 labelled, 4 negative). The test requires a real pgvector Postgres via `DB_URL` and otherwise skips.

The feed baseline in `backend/internal/rag/recall_integration_test.go` is the reference shape: `recallK = 10` measurement width against production `top_k = 5`, per-query `recall@1/@3/@5/@10` plus `hit@5`, `first_rank`, `mrr@5`, three scenarios (`corpus_only`, `scaled_noise`, `scaled_near`), 2000 decoys per scaled scenario, a 1000-row ANN floor, and a random-vector control. Its decoys are raw-SQL inserted rows owned by the fixture users, with noise and near-neighbour vector modes.

## Goals / Non-Goals

**Goals:**

- Make every labelled KB query's recalled number depend on ranking, not on the candidate set being smaller than `top_k`.
- Measure KB recall under the same competition class as the feed baseline: frozen corpus plus noise and near-neighbour decoys large enough to exercise the ANN path.
- Report the same metric family as the feed baseline so the two numbers can be compared at the same `K`.
- Keep the run deterministic, model-free, checked in, and green in the existing `backend-test` CI job.

**Non-Goals:**

- No production retrieval, chunking, indexing, config, or bridge changes.
- No `posts_douban` or other real user notes in the repository.
- No lexical/BM25 leg, reranker, or fusion-quality measurement; this change measures the dense retrieval leg only.
- No live embedding-model drift detection; the vectors remain a frozen snapshot.

## Decisions

### D1 — Fix the vacuous cases by growing the small directories, not by weakening the query

`q-caffeine-sleep` (`B-Sleep/`, 4 chunks), `q-teeth-routine` (`B-BodyCare/`, 3), and `q-travel-prep` (`P-Travel/`, 3) all filter to a directory with at most `top_k = 5` chunks, so `LIMIT 5` returns the entire candidate set and recall is 1.0 by construction. Grow each directory past `top_k` chunks by adding documents and sections to the fixture, keeping the original query texts and path filters. Relocating the queries to already-large directories was considered and rejected: it discards meaningful path-scope coverage (sleep/caffeine, oral care, travel) instead of fixing the corpus.

Add a fixture-validation rule that computes the eligible chunk count for every labelled query from the production chunker output and fails when a path-scoped query's directory holds `<= top_k` chunks, or when the whole fixture holds `<= top_k` chunks for an unscoped query. This is what prevents a future corpus edit from silently reintroducing a freebie.

### D2 — Expand the query set with categories that the current fixture does not stress

Add labelled queries in four categories: paraphrase-only semantic (no lexical overlap with the chunk), cross-directory same-topic distractor without a path filter (the corpus holds near-duplicate content in two directories and the query must rank the intended one), multi-relevant (2–3 gold chunks, exercising `recall@5 < 1.0` semantics), and buried (gold sits inside a long document with several similar sections). Target at least 30 labelled queries and at least 8 negatives, with every current category retained. The existing per-heading uniqueness rule stays: one heading path yields exactly one chunk, so one label maps to one chunk.

### D3 — Decoys are real KB rows in a second source, seeded by raw SQL

Retrieval joins `kb_chunks` to `kb_documents` and `kb_sources` and filters on owner, generation, `embedding_model`, source enabledness, and optional source/path. Decoys must therefore be real rows, not vectors attached to the fixture source. Insert a second directory source for the fixture user (`UpsertDirSource`, root = `t.TempDir()`), then batch-insert decoy documents and chunks with:

- `owner_id` = fixture user, `generation` = 1, `document.generation` = 1,
- `embedding_model` = `vectors.Model`,
- `rel_path` distributed under the fixture's top-level directories (`B-Sleep/decoy-…md`) so path-scoped queries also face decoys,
- synthetic `content`, `heading_path`, `locator`, `content_hash`, `token_count`,
- `embedding` written directly as a `vector(1024)` literal.

Raw SQL with `generate_series` batches (500 rows, matching the feed baseline) keeps the seed under a few seconds; going through `InsertChunk` + `SetChunkEmbedding` one row at a time would add thousands of round-trips, and a synthetic Markdown corpus would have to be embedded through the fake embedder for no measurement gain. The decoys are test scaffolding; the system under test remains the production retrieval SQL.

The decoy source is deleted in `t.Cleanup` (cascade removes documents and chunks) alongside the fixture source.

### D4 — Two decoy vector modes, seeded deterministically

- `noise`: random unit vectors, as in the feed baseline.
- `near`: take a randomly chosen fixture chunk vector as a base, add Gaussian noise scaled to a target cosine in `[0.35, 0.75]`, and normalize. This reproduces the feed baseline's `cos_to_base` band and forces the decoys to compete with real gold chunks in the top ranks.

A deterministic PRNG (`kbRecallRandomSeed`) keeps both modes reproducible. The decoy mode and count are reported per scenario. The vector helpers are duplicated from the feed test rather than exported from `internal/rag`: they are test scaffolding in two independent test packages, and exporting them would add production-visible API surface for test-only code.

### D5 — Three scenarios, owner-scoped for scaled runs

- `corpus_only`: the current shape — fixture source only, retrieval filtered by `source_id`.
- `scaled_noise` / `scaled_near`: fixture source plus the decoy source; retrieval filtered by owner only (`SourceID = nil`), so decoys are eligible.

Production retrieval is normally owner-scoped, not source-scoped; dropping the source filter in scaled scenarios is what lets decoys compete. Path-scoped queries keep their `PathPrefix`; because decoys are distributed under the same top-level directories, they compete there too.

Each scenario asserts the eligible chunk row count. A pinned `kbRecallAnnFloor = 1000` (mirroring the feed baseline's `annExercisedMinRow`) fails the run when a scaled scenario seeds fewer eligible rows. The default decoy count is 2000 per scaled scenario, overridable with `NYX_KB_RECALL_DECOYS` for local experiments.

### D6 — Measurement width 10, headline recall@5

Retrieve `k = 10` per query while the production result width stays `top_k = 5`. Per query compute `recall@1`, `recall@5`, `recall@10`, `hit@5` (binary any-hit within the first 5), and `first_rank` (1-based; 0 for a miss), plus the existing random-vector control. Aggregate by mean over labelled queries and by category. The regression gate is aggregate `recall@5` per scenario; `recall@10` and `first_rank` are diagnostic, so a gold chunk at rank 6–10 is distinguishable from one that was never retrieved.

Negative queries stay out of every recall figure; their top-1 scores are aggregated separately and asserted below a pinned ceiling, re-derived under the decoy scenarios because near decoys can raise a negative's top score.

### D7 — Floor derived from the hardened run, not carried over

Run the hardened corpus through all three scenarios, record the measured `recall@5` per scenario and per category in the design/docs, then pin the floor at `min(scenario recall@5) - margin`, with a small margin (0.02–0.05) because the frozen vectors and seeded decoys make the run deterministic apart from ANN graph state. The pre-hardening `0.85` floor is not reused. Every scenario is asserted against the same pinned floor; a scenario that falls below it fails with its observed value and the scenario name. The random-vector control must stay below the real-query recall in every scenario.

The measured baseline table and the regeneration commands go into `docs/knowledge-base.md`.

### D8 — CI fails, local skips

`requireKbRecallPgvector` keeps its local skip behavior (`SKIP_CONTAINERS=true`, empty `DB_URL`, or a database without pgvector), but when `CI` is set it must fail instead of skip: GitHub Actions sets `CI=true`, and the `backend-test` job already provides a `pgvector/pgvector:pg17` service with `DB_URL`. This prevents a misconfigured CI database from turning the gate into a silent skip.

## Risks / Trade-offs

- **Near decoys could dominate and tank recall** → target the feed baseline's `cos_to_base` band `[0.35, 0.75]`, keep the deterministic seed, derive the floor from the worst scenario, and report the decoy cosine range per scenario.
- **ANN search is not bit-for-bit deterministic** → small floor margin; decoys and vectors are seeded; the exact-vector `corpus_only` scenario remains as a stable reference.
- **Corpus growth changes chunk boundaries** → every added section stays inside the chunk budget, and the one-chunk-per-heading validation fails loudly if a section splits; frozen vectors are regenerated through the production chunk/embed path after the corpus edit.
- **Decoy seeding slows CI** → batched raw inserts (500 rows) keep both scaled scenarios in the same order of runtime as the feed baseline (which seeds 4000 rows in ~15 seconds).
- **Floor overfits a single measurement run** → record the per-scenario and per-category table, pin the floor with a margin below the worst scenario, and keep the random control and distinct-top-1 assertions as independent sanity checks.
- **The fixture is still synthetic and small relative to a real notes tree** → explicitly documented as a regression and comparability gate, not a generalization claim; a real-corpus opt-in eval remains a separate future change.

## Migration Plan

Test-only change: no migrations, no deploy step, no rollback path beyond reverting the test, fixtures, and docs. `kb_recall_vectors.json` is regenerated with `NYX_KB_RECALL_REGEN=1` against the local embedding server, then committed together with the corpus and docs. CI picks the change up through the existing `backend-test` job.
