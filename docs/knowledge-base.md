# Knowledge base (document RAG)

A second retrieval corpus beside the feeds: long Markdown documents, chunked by
heading and cited by path. The feed corpus is unchanged — `feed_embeddings` is
not modified and `rag: true` still means "my feeds".

## What it does

- **Sources.** One operator-managed *shared* directory readable by every user.
  Each user also gets an `inline` source holding documents uploaded or typed
  into the app.
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
- **Visibility and identity.** Retrieval is scoped to the shared source plus the
  caller's own sources on both sides of the join. Every chunk records the
  embedding model that produced its vector; a caller only sees chunks indexed
  with the same identity. A user whose BYOK model differs from the deployment's
  sees their own corpus but not the shared one — the UI explains this instead of
  showing an empty source.

## Endpoints

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/kb/sources` | Visible sources with counts, freshness, and indexed embedding identities |
| `PATCH` | `/api/kb/sources/{id}` | Enable or disable the caller's inline source |
| `DELETE` | `/api/kb/sources/{id}` | Remove the caller's inline source and everything indexed from it |
| `GET` | `/api/kb/documents` | List documents (`source_id`, `origin`, `status`, `limit`, `offset`) |
| `POST` | `/api/kb/documents` | Create a document: multipart (`file`) or JSON (`{name, content}`) |
| `GET` | `/api/kb/documents/{id}` | Document detail, content and heading paths |
| `PUT` | `/api/kb/documents/{id}` | Replace an inline document's content and re-index it |
| `DELETE` | `/api/kb/documents/{id}` | Delete an inline document and its chunks |
| `POST` | `/api/kb/index` | Index the caller's visible sources and fill pending documents |
| `GET` | `/api/kb/settings` | Corpus switch and the caller's embedding identity |
| `GET` | `/internal/rag/outline` | Service-to-service outline (JWT bearer; not in OpenAPI) |

`GET /internal/rag/retrieve` (the agent bridge) accepts `corpus=feeds|kb|both`,
plus optional `source` and `path` filters and `top_k`. `corpus=both` fuses the
two ranked lists with Reciprocal Rank Fusion — scores from the two corpora are
not comparable. A knowledge search also triggers the bounded lazy catch-up.

`/internal/rag/outline` returns the caller's visible sources, counts, and
top-level directory names for the planner; it carries no document content.

## Key environment variables

| Var | Default | Purpose |
|---|---|---|
| `KB_ENABLED` | `false` | Master switch. Off means no scan, no KB write, no KB route |
| `KB_ROOT` | empty | Shared directory; the source row is provisioned at boot, content is imported by `kb index` or the Index button |
| `KB_SHARED_SOURCE_NAME` | `shared` | Display name of the shared source |
| `KB_MAX_FILE_BYTES` | `2097152` | Per-file scan limit |
| `KB_MAX_FILES` / `KB_MAX_DEPTH` | `10000` / `20` | Per-scan bounds |
| `KB_CHUNK_TARGET_TOKENS` / `KB_CHUNK_MAX_TOKENS` | `400` / `700` | Chunk packing budget and hard cap |
| `KB_CHUNK_MIN_TOKENS` / `KB_CHUNK_OVERLAP_TOKENS` | `50` / `80` | Minimum section size (reserved) and overlap |
| `KB_TOP_K` / `KB_MAX_CONTEXT_CHARS` | `5` / `4000` | Retrieval result and rendered context size |
| `KB_INDEX_CONCURRENCY` | `2` | Concurrent source scans on the explicit Index path; chat catch-up remains sequential |

The numeric knobs are validated at boot: positive, bounded, and
`KB_CHUNK_MAX_TOKENS >= KB_CHUNK_TARGET_TOKENS > KB_CHUNK_OVERLAP_TOKENS`.

## Operator import

Bulk-import an existing corpus with the operator command. It shares the API
trigger's ingest pipeline and indexes under the deployment's embedding identity.

```bash
cd backend
KB_ENABLED=true KB_ROOT=/srv/nyx/kb/shared \
  go run ./cmd/kb index --root /srv/nyx/kb/shared

go run ./cmd/kb status
```

`kb index` creates a shared source, which makes the directory readable by every
user, so the command prints what it is about to expose before doing it. Without
an embedding configuration the command refuses to index with a message naming
the missing keys. `kb status` prints sources, document/chunk counts, pending
counts, and the deployment embedding identity as JSON.

## Recall baseline

`backend/internal/kb/recall_integration_test.go` pins a hand-labelled Chinese
corpus (`testdata/kb_recall_docs/`) with frozen 1024-dim vectors
(`testdata/kb_recall_vectors.json`). It covers path-scoped, heading-scoped,
exact-term, semantic, and negative queries, and recall@5 must stay at or above
the pinned floor. The fixture validates without a database; the retrieval run
needs pgvector.

```bash
cd backend
DB_URL=postgres://postgres:postgres@127.0.0.1:5433/nyx?sslmode=disable \
  go test ./internal/kb -run 'TestKbRecall'
```

Regenerate the frozen vectors after a chunker or embedding-model change:

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
bridge delta. No KB table is written unless the operator explicitly runs
`cmd/kb index` (an opt-in import). Enabling requires `LLM_ENABLED=true` (the KB
indexer shares the feed embedder); `KB_ROOT` is needed for the shared directory
source.

The chat panel's knowledge toggle applies to the non-agent chat path, the same
way the `rag` flag does. Under `AGENT_ENABLED=true` the Python planner decides
when to emit a `kb` step, using the knowledge outline.
