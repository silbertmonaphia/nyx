# Design

## Context

The feed RAG path is mature and measured. `internal/rag` owns an embedder (per-user BYOK resolution, SSRF-guarded dial, width check), an indexer (sha256 no-op skip, embed-on-write), a retriever (pgvector cosine top-K, user-scoped on both sides of the join), a service (embed → retrieve → render a truncated context block), and a bridge handler at `GET /internal/rag/retrieve` that the Python agent calls. `feed_embeddings` is a `feed_id`-keyed table with one row per feed — **there is no chunking**: `rag.ChunkText` is `Title + "\n\n" + Description`, and the whole thing becomes one vector. The recall baseline in `internal/rag/recall_integration_test.go` measures recall@5 = 0.944 over 68 hand-labelled Chinese queries.

A knowledge corpus breaks that shape on four axes at once: the unit is a file rather than a snippet, the text is long and structured rather than short and flat, the lifecycle is a filesystem rather than CRUD, and a passage must be attributable to a location. The reuse is real — same embedder, same BYOK resolution, same pgvector pool, same HNSW opclass, same bridge, same SSE frames — but the storage shape and the chunking are genuinely new work.

The corpus that motivates this is a categorized notes tree (`posts_douban`): 42 Markdown files under `A-`, `B-`, `P-`, `W-` prefixed directories, mostly Chinese, each file carrying a title and one to several `#`/`##` sections. That structure is the design driver: the directories are the natural retrieval scope, and the sections are the natural chunk boundary.

See proposal.md for motivation; behavior contract in specs/{kb-sources,kb-ingestion,kb-retrieval} and the deltas to specs/{agent-rag-bridge,agent-loop}.

## Goals / Non-Goals

**Goals:**

- A second corpus that coexists with the feed corpus without touching the feed vector table.
- Structure-preserving chunking that keeps a passage attributable to a file and a section.
- A source model that separates an operator-managed shared directory from per-user private directories, with private paths operator-confined.
- Reuse of the existing embedder, BYOK resolution, pgvector pool, bridge and SSE frames.
- An agent that can search the knowledge corpus deliberately — scoped to a source or a directory — and cite what it found.

**Non-Goals:**

- PDF / DOCX / HTML ingestion in this change. v1 is Markdown only; the seam for the others is designed here and specified, but the implementations land later.
- Lexical / BM25 second retrieval leg. The feed recall baseline found no measurable gap from a lexical leg on the feed corpus; a KB corpus is a different corpus, so the question is re-opened once there is a KB recall baseline to measure against. `pg_trgm` is already enabled in this deployment (migrations 000006/000007 gave the feed columns trigram indexes), so the leg is available later without a new extension — but Postgres `tsvector` is the wrong tool for a Chinese-dominant corpus, since it does not segment Chinese.
- Object storage for uploads. The corpus is small; a managed directory per user is enough, and the storage boundary is a seam.
- A shared/mega chunk table holding both corpora.
- Cross-corpus ranking calibration. Fusion is by rank (D8).
- Migration of the feed corpus to the new shape.

## Decisions

**D1 — The knowledge corpus is a separate corpus with its own tables; `feed_embeddings` is not touched.**
`feed_embeddings` is keyed by `feed_id` with a foreign key to `feeds`, its embedding column is `NOT NULL`, and it carries one HNSW index over one embedding column. A chunk row needs a document identity, a heading path, a locator, a nullable embedding (D4), and a per-chunk hash — none of which fit that shape without dropping the FK and rebuilding the index on a live table. Migration 000015 already paid the cost of an HNSW rebuild once, for a dimension change; paying it again to widen a table for a second corpus is a blast radius with no benefit.
*Alternative considered*: one polymorphic `chunks(source_type, source_id)` table serving both corpora — rejected. It loses the FK, forces one HNSW to serve two access patterns with different selectivity, and puts a migration on the live feed vector table to buy cross-corpus fusion that D8 solves at query time for free.

**D2 — Three tables: `kb_sources`, `kb_documents`, `kb_chunks`.**
`kb_sources` is "where content comes from, and who may see it": scope, owner, display name, root path, kind (`dir` | `upload`), enabled. `kb_documents` is one file: its source, scope/owner, relative path, title, type, content hash, byte size, mtime, status. `kb_chunks` is one embedded unit: its document, scope/owner/source (denormalised), ordinal, heading path, body, embedded text, hash, embedding, embedding model, token count, locator.
Why a source table rather than columns on the document: the visibility rule and the path-confinement base are properties of the *place*, not of each file; a user may have several private roots; and uploads need a synthetic per-user source so that an uploaded file and a scanned file are the same kind of thing.

**D3 — Visibility is `scope` + `owner_id`, denormalised onto both documents and chunks.**
A chunk is filtered on its own `scope`/`owner_id` columns — no join on the hot path, mirroring why `feed_embeddings` denormalises `user_id`. The retriever still JOINs `kb_documents` back and re-asserts that the document's scope and owner equal the chunk's, the same defence-in-depth the feed retriever applies (`queries/rag.sql` filters `fe.user_id`, `f.user_id` and `f.deleted_at IS NULL`). Rule: `scope = 'shared'` is visible to every caller; `scope = 'user'` is visible to its owner only.
*Alternative considered*: derive visibility by joining `kb_sources` — rejected for the hot path, kept as the source of truth that the denormalised columns must agree with.

**D4 — Index before embed: chunks are written with a null embedding, and a second pass fills them.**
The feed path embeds before insert because a feed is one vector and the write is user-visible: a failed embed must not leave a half-written feed. A knowledge ingest produces hundreds of chunks from one file; if the embedding call fails halfway, re-running the parse is pure waste, and the user has already waited. So the parse and the chunk write happen first, the embedding fills afterwards through a claim (`UPDATE … WHERE embedding IS NULL … RETURNING`, skippable by other workers), and a failure leaves the document `pending` with its chunks already stored.
Cost: `kb_chunks.embedding` is nullable, unlike `feed_embeddings.embedding`, and the retrieval query filters `embedding IS NOT NULL`. Both are explicit in the delta. This also makes the fill pass the natural place to enforce the embedding-identity rule that `feed-embed-indexing` already owns for feeds — when the user's embedding model changes, the KB vectors are invalidated the same way.

**D5 — Chunking is heading-aware, and the embedded text carries a context header.**
Rules: a chunk belongs to exactly one heading path and never spans two; within a path, blocks are packed to a target token budget under a hard cap; tables are never split (an oversized table splits by rows with the header repeated); fenced code blocks are never split; a section below the minimum size is kept rather than dropped and still carries its heading; overlap does not cross a heading boundary.
The embedded text is `rel_path › heading_path` followed by the body; the body alone is stored separately for citation. This is the single largest recall lever available: a chunk from `B-Cook/2019-11-16-Cook-Diet.md › 煮食记 / 碳水列表` embeds with that prefix, so its vector carries *where* the text came from, not just what it says. Without it, a chunk saying "一份碳水 + 一份蛋白 + 两份蔬菜" is nearly indistinguishable from any other chunk saying the same thing.
Budget: the local embed server runs with `--max-model-len 2048`, so the hard cap (default 700 tokens) leaves ample headroom, and the target (default 400) keeps most chunks well clear of it.
*Alternative considered*: fixed-size sliding windows — rejected. They slice across heading boundaries, discard the section semantics that make the corpus navigable, and produce chunks whose vectors say nothing about where they came from.

**D6 — One normalizer seam; Markdown is the only v1 implementation.**
`Normalizer{Handles(docType) bool; Normalize(ctx, raw, src) (*Document, error)}` returns a flat, ordered block list whose blocks carry a heading path, a kind, and a locator. v1 registers only the Markdown implementation. The chunker, the store, the indexer and the retrieval path are written against the block list, not against Markdown, so adding a format later adds one implementation and one `docType` case — nothing else moves.
Runtime: Go for v1. The PDF/DOCX ecosystems in Go are weak (pdfcpu loses heading structure; `lufia/docx` loses styles and tables), so the later formats are planned as a normalizer implemented in the Python service behind a new route, reached by Go over the service token — the Go→Python direction already exists (`chat.NewAgentProxy`, `X-Nyx-Service-Token`), so no new deployment unit is introduced.
v1 deliberately ships **no binary parser**: an untrusted PDF/DOCX parser is a materially different attack surface from a text normalizer (zip bombs, decompression bombs, image decoders), and it is not needed to prove the pipeline. That decision also keeps the path-validation story simple — the only filesystem content the v1 server reads is text it will display as text.

**D7 — Change detection is at the document level; chunk hashes are stored for a later refinement.**
The document's content hash is sha256 of its raw bytes. Unchanged → skip the document entirely, zero embedding calls (the same no-op guard the feed indexer uses). Changed → delete the document's chunks and re-chunk and re-embed the whole file. Removed from disk → its chunks go away and it stops being retrievable.
Why document-level for v1: heading-aware chunking has no stable identity for "this chunk, across edits". Chunk *n* of the old file may correspond to a different region of the new file, so a chunk-level diff would need content-addressed chunk identity plus heading-path alignment to be correct. Storing the per-chunk hash now means that refinement is a query change later, not a re-parse of history.

**D8 — Corpus selection is explicit; `both` fuses by rank, not by score.**
The bridge takes `corpus=feeds|kb|both` (absent means `feeds`). `feeds` is byte-identical to today. `kb` searches the knowledge corpus with optional `source` and `path` filters. `both` takes the top-K from each corpus and combines them with Reciprocal Rank Fusion.
Why RRF: the two corpora are not on a comparable similarity scale. A feed snippet and a document chunk have different lengths, different topical spread, and different score distributions, so `score_a > score_b` across corpora is not a meaningful comparison. RRF consumes only each passage's *rank within its own corpus*, which is scale-free and needs no calibration constant.
*Alternative considered*: one vector query across a merged table — rejected (D1). *Alternative considered*: normalising the two score lists (z-score / min-max) before merging — rejected as a fitted constant with no principled value; rank fusion has none.

**D9 — The citation envelope becomes a tagged union, extended additively.**
Today each citation is `{feed_id, snippet}` and the shape is frozen in `agent-rag-bridge`. A knowledge citation needs the source name, the relative path, the heading path, and the locator. The envelope keeps `context_block` and `citations`; each citation gains a `kind` discriminator (`feed` | `doc`); a feed citation keeps its identifier in `feed_id`; a knowledge citation carries `feed_id: 0` plus `doc_id`, `source_name`, `rel_path`, `heading_path`, `locator`. The Python client keeps reading `citations[].feed_id`, so the existing consumer is not broken, and the existing "never `null`" rule is preserved.
Because this is a spec-frozen wire shape, it ships as a delta to `agent-rag-bridge`. Per the repo's wire-contract rule, the change must include a test in which the **Python** parser consumes the **Go** producer's bytes — two sides each asserting their own shape is the classic way both sides stay green while both are wrong.

**D10 — The agent gets a new `kb` step kind rather than a corpus parameter on `rag`.**
`agent-loop`'s step-kind requirement names `rag` as "vector retrieval over feeds" and the planner prompt repeats that `rag` is for the user's feed data. A corpus parameter would make one step kind mean two things and would make the `step_start` frame ambiguous about what was searched. A distinct `kb` kind keeps the plan self-describing, keeps the planner prompt honest, and reuses the existing `step_start`/`step_end` frames — no new SSE frame kinds.
A `kb` step carries `query`, optionally `source` and `path`. The planner is given a knowledge outline (per visible source: id, name, scope, document count, chunk count, top-level directory names) so it can narrow a query to a directory instead of searching everything. Without the outline the planner cannot know that `B-Sport/` exists, and the directory taxonomy — the main organising principle of a notes tree — would be invisible to it.

**D11 — Indexing triggers: explicit, plus a bounded lazy catch-up.**
Explicit trigger: the caller asks for an index over the sources visible to them (their private sources plus the shared source). The hash skip makes a repeat scan cheap — a rescan of an unchanged tree costs file reads and hashes, no embedding calls.
Lazy catch-up: a chat request that observes a `pending` document or an unembedded chunk starts a background fill on a detached context, bounded per invocation. This is deliberately the same shape as `rag.Service.BackfillOnce` — a detached context, a per-user once-guard, a bounded batch — because it is the same problem: the user should not wait for indexing to answer a question.
*Alternative considered*: a filesystem watcher — rejected. Bind mounts, network filesystems and WSL all make inotify unreliable, and the hash-based scan is already cheap at this scale. *Alternative considered*: a periodic ticker — deferred; it adds a background loop, a scan lock and a failure mode that the explicit trigger plus catch-up already covers.

**D12 — Upload is a write into the user's own managed root, not a second storage model.**
`POST /api/kb/documents` accepts a Markdown file. The bytes are sniffed as text/Markdown — the extension and the client's declared content type are not trusted — then written under the user's managed directory, registered as a document, and indexed inline with a bounded timeout. Because the file lands in a directory the scanner also walks, the content hash prevents the same content from being indexed twice; the upload path and the scan path converge on one document row and one set of chunks.
Object storage is deferred deliberately: the corpus is small, the storage boundary is the seam, and a managed directory is one fewer moving part than a bucket plus credentials plus a signed-URL story.

## Risks / Trade-offs

- [A private root is a TOCTOU window between save and scan] → validated at save time AND re-validated at scan time; the scan resolves the real path again and refuses anything outside the base, so a root replaced by a symlink after saving is caught at the moment it is used. The cost is that a legitimate reorganisation of the private base breaks the source until the path is re-saved — which surfaces as a recorded reason on the source, not a silent empty result.
- [Document-level re-chunking re-embeds a whole file on any edit] → acceptable at knowledge-corpus scale (a file is typically under a dozen chunks). The per-chunk hash is stored so a later refinement can narrow the re-embed to the chunks whose content actually changed.
- [Two corpora means two places for the model to look] → the planner decides, and the outline makes the decision informed. The alternative — silently merging the corpora into one result list — would hide the distinction from the user and make the citation ambiguous.
- [Fusion by rank discards score information] → deliberately. The scores are not comparable across corpora (D8), and rank is the part that is comparable. A single-corpus request still returns scores.
- [The embedding model identity has to be tracked per chunk] → `kb_chunks.embedding_model` exists for exactly the reason `mem0_prep.md` §6 records for preferences: a silent model swap desyncs a vector store, and the desynced rows must be findable, not discoverable by accident.
- [Chinese exact-term recall may be weaker in a long-form corpus than in a snippet corpus] → not asserted either way without a measurement. The change ships a KB recall baseline with hand-labelled queries so the question can be answered with data; if it turns out a lexical leg is needed, `pg_trgm` is already available and the retrieval path has a filter seam for it.
- [A second vector store doubles the operational surface] → accepted. The alternative (one table for both corpora) puts a migration on the live feed vector table, which is the larger operational risk.

## Migration Plan

Default-off: `KB_ENABLED` unset means no KB tables are written, no source is scanned, and no knowledge retrieval is offered — behaviour is identical to today. Enabling requires the operator to set `KB_ROOT` (shared directory), `KB_PRIVATE_BASE` (per-user confinement root) and `KB_UPLOAD_ROOT`, and to make those paths readable by the backend process; the migration itself adds three tables and one HNSW index and does not modify `feed_embeddings`. Rollback is to unset `KB_ENABLED` and restart — the tables can be left in place, and dropping them is a separate, explicit operation.
Changing the user's embedding identity invalidates their stored vectors; the KB follows the same rule as feeds (`feed-embed-indexing`, "Changing the embedding identity invalidates stored vectors"), so the KB's invalidation hook runs alongside the feed one when the embedding configuration is replaced.

## Open Questions

None for this change. Deferred to the follow-up that adds non-Markdown formats: whether the PDF/DOCX normalizer runs in the Python service as planned (D6) or in a separate sandboxed worker, and whether a scanned PDF without a text layer is rejected or routed to OCR.
