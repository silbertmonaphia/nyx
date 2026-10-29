# Proposal

## Why

Feed RAG retrieves over a corpus of short, self-authored snippets: one feed is one vector, embedded whole, with no chunking at all. The knowledge a user actually keeps — a categorized notes tree like `posts_douban` (`B-Cook/2019-11-16-Cook-Diet.md`, `B-Sport/2022-10-18-Sport-MMA-Submission-BJJ.md`), and later HTML/PDF/Word documents — is long, structured, and organized by directory. None of it can be expressed by the current feed path: a whole-file feed would either blow past the embedding window or lose its section semantics, and there is nowhere to record where a passage came from.

So the retrieval surface needs a second corpus: documents that are chunked, indexed in bulk, scoped by source, and cited by path. The corpus is the user's own; the plumbing (embedder, per-user BYOK, pgvector, the agent bridge) already exists and should be reused rather than rebuilt.

## What Changes

- **A second corpus, with its own tables.** `kb_sources` (where content comes from + who may see it), `kb_documents` (one file), `kb_chunks` (one embedded unit). `feed_embeddings` is untouched — no migration on the live feed vector table.
- **Two source scopes.** One operator-managed *shared* directory readable by every user, plus per-user *private* directories whose paths are confined to an operator-configured base (`KB_PRIVATE_BASE`). Private roots are validated at save time and again at scan time (symlink / TOCTOU escape refused).
- **Two editing models, one pipeline.** A document's *origin* decides where its truth lives. A `file` document (from a directory source) is authoritative on disk — the application refuses to edit it and points the user at the source file. An `inline` document (uploaded or created in the application) stores its body with the document and is editable in place, keeping its identity across the edit.
- **Upload stores the body inline.** `POST /api/kb/documents` accepts a Markdown file, sniffs its content, stores it as an inline document owned by the uploader, and indexes it. There is no upload sink on disk and no second storage model.
- **Heading-aware chunking instead of one-vector-per-item.** Markdown is parsed into a heading tree; chunks never span two heading paths; tables and fenced code blocks are never split; every chunk embeds with a context header (relative path + heading path) so its vector carries its provenance, and stores its body separately for citation.
- **Index-before-embed, so a failed embed never re-parses and never empties the corpus.** Chunks land with a null embedding and are filled by a resumable claim pass; unchanged files cost zero embedding calls (content-hash skip, mirroring the feed indexer's no-op guard). A document carries a *generation*, and it advances to its new content only once the new chunks are fully embedded — so a document whose re-embed fails stays retrievable at its previous content instead of vanishing from search.
- **Manual index trigger plus a bounded lazy catch-up.** An explicit index action over the caller's sources; a background catch-up at chat time that completes pending documents on a detached context — the same shape as the existing feed lazy backfill.
- **Explicit corpus selection on the bridge.** `GET /internal/rag/retrieve` gains `corpus=feeds|kb|both`, plus optional `source` and `path` filters. `corpus=feeds` is byte-identical to today. `corpus=both` fuses the two result lists by rank (Reciprocal Rank Fusion), not by score — the two corpora are not on a comparable cosine scale.
- **Citations become a tagged union.** KB passages cite their source name, relative path, heading path and locator alongside the existing feed citations; the envelope keeps `context_block` and `citations`.
- **The agent gets a `kb` step kind**, not a corpus parameter on `rag`: `rag` stays "the user's feeds", `kb` searches the knowledge base with optional source / path scoping, and the planner is given a KB outline so it can narrow a query to a directory instead of guessing.
- **A `kb` toggle on the chat request** for the non-agent chat path, alongside the existing `rag` toggle.

Not changing: the feed corpus, the feed write path, the feed indexer's semantics, the `rag` step kind's meaning, the SSE frame set (no new frame kinds — `kb` reuses `step_start` / `step_end`), the MCP tool registry.

## Capabilities

### New Capabilities

- `kb-sources`: the registry of where knowledge content comes from and who may see it — shared operator directories, per-user private directories confined to an operator-configured base, the per-user inline store, source enable/disable, and per-source freshness and content counts.
- `kb-ingestion`: the document → chunk → embedding pipeline — Markdown normalization into a heading-aware block sequence, heading-aware chunking with a context header, content-hash change detection, index-before-embed with a resumable fill pass and an atomic generation swap, the origin-driven edit model, manual trigger plus bounded lazy catch-up, and observable per-document status.
- `kb-retrieval`: retrieval over the knowledge corpus — visibility scoping (shared plus the caller's own private sources), source and path-prefix filters, explicit corpus selection with rank-based fusion, KB citation provenance, and the KB outline read model.

### Modified Capabilities

- `agent-rag-bridge`: the retrieval endpoint gains corpus selection and source/path filters, and the citation envelope gains KB citations alongside feed citations (additive fields plus a `kind` discriminator — the shape is frozen in that spec, so it ships as a delta).
- `agent-loop`: the enumerated step kinds gain `kb` (vector retrieval over the knowledge corpus), and a `kb` step carries its query and optional scope arguments.

## Impact

- `backend/migrations/`: new `000018_create_kb_sources_documents_chunks` (three tables, one HNSW index) — golang-migrate owns the DDL, alembic stays the owner of the `agent.*` schema, one owner per table.
- `backend/queries/kb.sql` + `backend/internal/kb/db/` (sqlc output, regenerated with `make sqlc`).
- `backend/internal/kb/`: new package — source registry, normalizer, chunker, connector, indexer, retriever, outline.
- `backend/internal/rag/`: the service gains corpus selection and rank fusion; the bridge handler gains the new query parameters and the extended citation envelope. The feed facade is unchanged.
- `backend/internal/chat/`: the request body gains `kb`; the service injects KB context alongside (or instead of) feed context.
- `backend/internal/platform/config/`: new `KB_*` keys, default-off.
- `backend/agent_py/agent/`: `types.py` (step kind), `planner.py` (step catalogue + outline injection), `executor.py` (`_run_kb`), `rag_client.py` (corpus + filters).
- `frontend/src/features/kb/`: new feature slice — knowledge-base page (sources, documents, status, upload, index trigger), a document view/edit page for inline documents (the edit control for a file-backed document is disabled with a pointer at the source file), plus the private-directory field in the settings modal and the chat toggle.
- `evals/` + `frontend/tests/e2e/`: a KB recall baseline and an end-to-end spec.
- New Go dependency: a Markdown parser (pure Go, no cgo). No Python dependency in this change.
- Rollout gates: none foreseen — the change is default-off (`KB_ENABLED=false`), behaviour-neutral when unset, and dev/CI-completable end to end (implementation, unit tests, pgvector integration test, recall baseline, and a Playwright e2e against the dev stack), so per the rollout-split classification test there is **no sibling rollout change**.
