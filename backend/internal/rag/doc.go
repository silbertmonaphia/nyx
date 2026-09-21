// Package rag is the retrieval-augmented generation layer over the
// feed domain. It owns:
//   - Embedder: wraps llm.Provider.Embed; computes the canonical
//     chunk_text (Title + "\n\n" + Description) and its sha256 hash.
//   - Indexer: writes embeddings into feed_embeddings on feed writes
//     (Create/Update/Delete from the feed domain); skips the Embed
//     API call when content_hash is unchanged.
//   - Retriever: top-K semantic lookup for the authenticated user
//     using the existing feed_embeddings HNSW index.
//   - Service: orchestrates the chat-side path — embed query,
//     retrieve, render a context block, and run the lazy backfill
//     (sync.Map + sync.Once guards a per-user at-most-once run).
//
// The package depends on llm.Provider for embeddings (so the same
// failover Router that wraps chat also wraps embeddings) and on
// feed.Feed for the canonical row shape. The feed domain does NOT
// depend on rag — the dependency is inverted via a narrow
// EmbeddingIndexer interface declared in the feed package.
package rag