# Knowledge base (document RAG)

A second retrieval corpus beside the feeds: long Markdown documents, chunked by
heading and cited by path. The feed corpus is unchanged — `feed_embeddings` is
not modified and `rag: true` still means "my feeds".

## What it does

- **Sources.** Every source belongs to exactly one user. A user's `inline`
  source holds documents uploaded or typed into the app; directory (`dir`)
  sources are registered per user by the operator CLI (`kb index --user`) and
  are never exposed through the HTTP API. Excluding a source from search skips
  its scans and keeps it out of retrieval, but leaves its documents listed and
  editable in the SPA so the source can be re-enabled.
- **Chunking.** Markdown is parsed into a heading-aware block sequence. A chunk
  never spans two heading paths; tables and fenced code blocks are never split;
  every embedded text starts with `relative/path › heading / path` while the
  stored body stays clean for citation.
- **Index before embed.** Chunks are written with a null embedding and filled by
  a bounded fill pass; the claim is best-effort, so overlapping workers may
  repeat a chunk, but the write is idempotent. A document advances to its
  new content only when the new generation is fully embedded, so a failed embed
  never removes a document from search. Unchanged files cost zero embedding
  calls.
- **Two editing models.** A `file` document is authoritative on disk: the API
  refuses edits and deletes with a static message and points at the source file.
  An `inline` document stores its body with the row and is editable in place,
  keeping its identifier.
- **Visibility and identity.** Retrieval is scoped to the caller's own sources,
  re-asserted across the chunk, document, and source rows. Every chunk records
  the embedding model that produced its vector; a caller only sees chunks
  indexed with the same identity. Changing the user's embedding model strands
  the existing chunks: delete and re-register the directory source (or delete
  and recreate inline documents) to re-index under the new model. The UI shows
  the identity mismatch on the source card instead of an empty corpus.

## Endpoints

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/kb/sources` | The caller's sources with counts, freshness, and indexed embedding identities |
| `PATCH` | `/api/kb/sources/{id}` | Enable or disable a source the caller owns |
| `DELETE` | `/api/kb/sources/{id}` | Remove a source the caller owns and everything indexed from it |
| `GET` | `/api/kb/documents` | List documents (`source_id`, `origin`, `status`, `limit`, `offset`) |
| `POST` | `/api/kb/documents` | Create a document: multipart (`file`) or JSON (`{name, content}`) |
| `GET` | `/api/kb/documents/{id}` | Document detail, content and heading paths |
| `PUT` | `/api/kb/documents/{id}` | Replace an inline document's content and re-index it |
| `DELETE` | `/api/kb/documents/{id}` | Delete an inline document and its chunks |
| `POST` | `/api/kb/index` | Index the caller's enabled sources and fill pending documents (API only; the SPA has no index button) |
| `GET` | `/api/kb/settings` | Knowledge-base availability and the caller's embedding identity |
| `GET` | `/internal/rag/outline` | Service-to-service outline (JWT bearer; not in OpenAPI) |

`GET /internal/rag/retrieve` (the agent bridge) accepts `corpus=feeds|kb|both`,
plus optional `source` and `path` filters and `top_k`. `corpus=both` fuses the
two ranked lists with Reciprocal Rank Fusion — scores from the two corpora are
not comparable. A knowledge search also triggers the bounded lazy catch-up.

`/internal/rag/outline` returns the caller's own sources, counts, and top-level
directory names for the planner; it carries no document content and names no
other user's source.

## Key environment variables

| Var | Default | Purpose |
|---|---|---|
| `KB_ENABLED` | `false` | Master switch. Off means no scan, no KB write, no KB route |
| `KB_MAX_FILE_BYTES` | `2097152` | Per-file scan limit for directory sources |
| `KB_MAX_FILES` / `KB_MAX_DEPTH` | `10000` / `20` | Per-scan bounds |
| `KB_CHUNK_TARGET_TOKENS` / `KB_CHUNK_MAX_TOKENS` | `400` / `700` | Chunk packing budget and hard cap |
| `KB_CHUNK_MIN_TOKENS` / `KB_CHUNK_OVERLAP_TOKENS` | `50` / `80` | Minimum section size (reserved) and overlap |
| `KB_TOP_K` / `KB_MAX_CONTEXT_CHARS` | `5` / `4000` | Retrieval result and rendered context size |
| `KB_INDEX_CONCURRENCY` | `2` | Concurrent source scans on the explicit Index path; chat catch-up remains sequential |

The numeric knobs are validated at boot: positive, bounded, and
`KB_CHUNK_MAX_TOKENS >= KB_CHUNK_TARGET_TOKENS > KB_CHUNK_OVERLAP_TOKENS`.

## Operator import

Register a directory as one user's private source and index it with that user's
BYOK embedding configuration. The command shares the API ingest pipeline; the
path never crosses the HTTP boundary, so there is no caller-facing source
creation and no path confinement setting.

```bash
cd backend
go run ./cmd/kb index --user <user-id> --root /srv/nyx/kb/posts_douban

go run ./cmd/kb status
```

`--name` defaults to the directory's base name and is unique per user; `inline`
is reserved. Re-running the same name repoints that user's directory source.
The CLI needs the same `DB_URL` and `USER_LLM_ENCRYPTION_KEY` as the backend,
and the target user must have saved an embedding configuration — otherwise the
command refuses to index. `kb status` prints `owner_id`, kind, and
document/chunk/pending counts per source as JSON.

Directory contents change on disk independently; re-run `kb index` (or call
`POST /api/kb/index`, which is not surfaced by the SPA) to sync them. Unchanged
files cost no embedding calls, and an interrupted run resumes from the database
state when re-run.

## Recall baseline

`backend/internal/kb/recall_integration_test.go` pins a hand-labelled Chinese
corpus (`testdata/kb_recall_docs/`, 26 documents / 89 chunks) with frozen
1024-dim vectors (`testdata/kb_recall_vectors.json`). The corpus has 64 labelled
queries and 12 negative queries across path-scoped, heading-scoped, exact-term,
semantic, paraphrase, same-topic distractor, multi-relevant, buried,
vague-referential, cluster-confusable, opaque-identifier, and symptom
categories. Category split: path 5, heading 6, exact 4, semantic 6, paraphrase
6, distractor 3, multi 5, buried 6, vague 5, cluster 5, opaque 8, symptom 5.
The `opaque_id` set has four contextual queries and four bare-identifier
probes (`Op.27 No.2`, `哈农第 39 条`, `配方 B-12`, `P-0412`); the bare probes
carry no topical context, so the identifier alone must carry the retrieval.
Four negatives target topics adjacent to the corpus (supplements, knee braces,
coffee-machine descaling, sleep-mask brands). A labelled query is rejected at
fixture-validation time when its retrieval scope holds at most `top_k = 5`
chunks, because `LIMIT 5` would then return the whole candidate set and recall
would be 1.0 regardless of ranking. The fixture validates without a database;
the retrieval run needs pgvector.

Measurement uses `k = 10` with the production result width left at `top_k = 5`.
Each scenario reports `recall@1`, `recall@5`, `recall@10`, `hit@5`, mean
`first_rank`, a fixed random-vector control, the negative-query top-1 mean, and
the distinct top-1 count. The headline regression metric is `recall@5`; the
`recall@10`/`first_rank` pair separates a gold chunk at rank 6–10 from one that
was not retrieved.

Three scenarios run: `corpus_only` filters by the fixture source; `scaled_noise`
and `scaled_near` add 2000 decoy chunks (override with
`NYX_KB_RECALL_DECOYS`) and retrieve owner-scoped, so decoys compete. Decoys are
distributed under the fixture's top-level directories; near decoys sit in a
cosine band of `[0.35, 0.75]` against a fixture chunk. A scaled scenario with
fewer than 1000 eligible chunk rows fails instead of reporting a recall measured
on a table too small to exercise the index.

Measured baseline (frozen vectors, `Qwen/Qwen3-Embedding-0.6B`):

| Scenario | Recall@1 | Recall@5 | Recall@10 | Hit@5 | Mean first rank | Random@5 | Negative top-1 | Rows | Distinct top-1 |
|---|---|---|---|---|---|---|---|---|---|
| `corpus_only` | 0.836 | 0.992 | 1.000 | 1.000 | 1.172 | 0.102 | 0.426 | 89 | 46/64 |
| `scaled_noise` | 0.836 | 0.992 | 1.000 | 1.000 | 1.172 | 0.000 | 0.426 | 2089 | 46/64 |
| `scaled_near` | 0.836 | 0.984 | 1.000 | 1.000 | 1.172 | 0.000 | 0.426 | 2089 | 46/64 |

Per-category `recall@5` is 1.000 for every category except multi-relevant:
0.900 in `corpus_only`/`scaled_noise` and 0.800 in `scaled_near`, where the
second gold chunks of `q-multi-mealprep` (all scenarios) and `q-multi-insurance`
(`scaled_near` only) rank 6–10; `recall@10` is 1.000 everywhere. The eight
opaque-identifier queries all rank 1 in every scenario, including the four
bare-identifier probes, whose top-1 scores are 0.455–0.669. The pinned
`recall@5` floor is **0.95** (worst measured scenario 0.984 minus a 0.034
margin) and applies to every scenario. The negative top-1 ceiling is **0.48**
(measured 0.426 with the near-miss negatives).

The retrieval run needs pgvector; the baseline fails closed when `CI` is set and
`DB_URL` or pgvector is missing, and skips locally. An explicit
`SKIP_CONTAINERS=true` is a deliberate opt-out that also skips under `CI`, as the
`make coverage-check` recipe does.

```bash
cd backend
DB_URL=postgres://postgres:postgres@127.0.0.1:5433/nyx?sslmode=disable \
  go test ./internal/kb -run 'TestKbRecall' -v
```

Regenerate the frozen vectors after a chunker, corpus, or embedding-model
change:

```bash
NYX_KB_RECALL_REGEN=1 \
NYX_KB_RECALL_EMBED_URL=http://127.0.0.1:8001/v1 \
NYX_KB_RECALL_EMBED_MODEL=Qwen/Qwen3-Embedding-0.6B \
  go test ./internal/kb -run TestRegenerateKbRecallVectors
```

The live Playwright spec is `frontend/tests/e2e/kb-rag.spec.ts`; it runs in the
model-dependent suite:

```bash
cd frontend
npm run test:e2e:live -- tests/e2e/kb-rag.spec.ts
```

## Default-off behaviour

With `KB_ENABLED` unset: no KB route is mounted, no source is scanned, and the
feed retrieval passages, ordering, and context block are identical to before.
The SPA probes `GET /api/kb/settings` and hides the Knowledge tab when the
route is absent, so a deployment with the corpus off never renders the page.
Feed citations gain the additive `kind: "feed"` discriminator required by the
bridge delta. No KB table is written unless a user uploads a document or the
operator registers a directory with `cmd/kb index`. Enabling requires
`LLM_ENABLED=true` because indexing uses each user's saved BYOK embedding
configuration.

The chat panel's knowledge toggle applies to the non-agent chat path, and its
sibling feeds toggle sends `rag: true` on that same path. Under
`AGENT_ENABLED=true` the SPA hides both toggles (the authenticated
`GET /api/features` probe reports `agent_enabled`) and the Python planner
decides when to emit a `kb` or `rag` step; `kb` steps use the knowledge outline.
