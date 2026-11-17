-- name: GetSource :one
SELECT * FROM kb_sources WHERE id = $1;

-- name: ListSources :many
SELECT * FROM kb_sources
WHERE owner_id = $1
ORDER BY name;

-- name: ListAllSources :many
SELECT * FROM kb_sources
ORDER BY owner_id, name;

-- name: UpsertDirSource :one
INSERT INTO kb_sources (owner_id, name, kind, root_path)
VALUES ($1, $2, 'dir', $3)
ON CONFLICT (owner_id, name) DO UPDATE SET
    root_path  = EXCLUDED.root_path,
    enabled    = TRUE,
    updated_at = CURRENT_TIMESTAMP
WHERE kb_sources.kind = 'dir'
RETURNING *;

-- name: EnsureInlineSource :one
INSERT INTO kb_sources (owner_id, name, kind)
VALUES ($1, 'inline', 'inline')
ON CONFLICT (owner_id, name) DO UPDATE SET
    updated_at = CURRENT_TIMESTAMP
WHERE kb_sources.kind = 'inline'
RETURNING *;

-- name: SetSourceEnabled :one
UPDATE kb_sources
SET enabled = $2, updated_at = CURRENT_TIMESTAMP
WHERE id = $1 AND owner_id = $3
RETURNING *;

-- name: DeleteSource :execrows
DELETE FROM kb_sources WHERE id = $1 AND owner_id = $2;

-- name: UpdateSourceScan :exec
UPDATE kb_sources
SET last_scan_at = CURRENT_TIMESTAMP,
    last_error   = $2,
    updated_at   = CURRENT_TIMESTAMP
WHERE id = $1;

-- name: SourceDocumentCount :one
SELECT COUNT(*) FROM kb_documents WHERE source_id = $1;

-- name: SourceChunkCount :one
SELECT COUNT(*)
FROM kb_chunks c
JOIN kb_documents d ON d.id = c.document_id
WHERE c.source_id = $1 AND c.generation = d.generation;

-- name: SourcePendingCount :one
SELECT COUNT(*) FROM kb_documents WHERE source_id = $1 AND status <> 'indexed';

-- name: ListTopLevelDirectories :many
SELECT DISTINCT split_part(rel_path, '/', 1)::text AS directory
FROM kb_documents
WHERE source_id = $1 AND position('/' in rel_path) > 0
ORDER BY directory;

-- name: GetDocument :one
SELECT * FROM kb_documents WHERE id = $1;

-- name: ListDocumentsByOwner :many
SELECT * FROM kb_documents d
WHERE d.owner_id = sqlc.arg(owner_id)::bigint
  AND (sqlc.narg(source_id)::bigint IS NULL OR d.source_id = sqlc.narg(source_id)::bigint)
  AND (sqlc.narg(origin)::text IS NULL OR d.origin = sqlc.narg(origin)::text)
  AND (sqlc.narg(status)::text IS NULL OR d.status = sqlc.narg(status)::text)
ORDER BY d.rel_path, d.id
LIMIT sqlc.arg(limit_count)::int OFFSET sqlc.arg(offset_count)::int;

-- name: SourceEmbeddingModels :many
SELECT DISTINCT embedding_model FROM kb_chunks
WHERE source_id = $1 AND embedding_model <> ''
ORDER BY embedding_model;

-- name: GetOwnedDocument :one
SELECT * FROM kb_documents
WHERE id = $1 AND owner_id = $2;

-- name: GetDocumentBySourcePath :one
SELECT * FROM kb_documents WHERE source_id = $1 AND rel_path = $2;

-- name: GetInlineDocumentByHash :one
SELECT * FROM kb_documents
WHERE origin = 'inline' AND owner_id = $1 AND content_hash = $2
ORDER BY id
LIMIT 1;

-- name: ListDocumentsBySource :many
SELECT * FROM kb_documents
WHERE source_id = $1
ORDER BY rel_path
LIMIT $2 OFFSET $3;

-- name: ListPendingDocuments :many
SELECT d.*
FROM kb_documents d
JOIN kb_sources s ON s.id = d.source_id
WHERE d.status = 'pending'
  AND s.enabled
  AND d.owner_id = $1
ORDER BY d.updated_at
LIMIT $2;

-- name: ListDocumentsWithUnembeddedChunks :many
SELECT DISTINCT d.*
FROM kb_documents d
JOIN kb_sources s ON s.id = d.source_id
JOIN kb_chunks c ON c.document_id = d.id AND c.embedding IS NULL
WHERE s.enabled
  AND d.owner_id = $1
ORDER BY d.updated_at
LIMIT $2;

-- name: UpsertFileDocument :one
INSERT INTO kb_documents (
    source_id, owner_id, origin, rel_path, title, doc_type,
    content_hash, byte_size, mtime, status, error_detail, generation, content
)
VALUES ($1, $2, 'file', $3, $4, $5, $6, $7, $8, 'pending', '', 0, NULL)
ON CONFLICT (source_id, rel_path) DO UPDATE SET
    content_hash = EXCLUDED.content_hash,
    byte_size    = EXCLUDED.byte_size,
    mtime        = EXCLUDED.mtime,
    title        = EXCLUDED.title,
    doc_type     = EXCLUDED.doc_type,
    status       = 'pending',
    error_detail = '',
    updated_at   = CURRENT_TIMESTAMP
RETURNING *;

-- name: UpsertInlineDocument :one
INSERT INTO kb_documents (
    source_id, owner_id, origin, rel_path, title, doc_type,
    content_hash, byte_size, mtime, status, error_detail, generation, content
)
VALUES ($1, $2, 'inline', $3, $4, 'markdown', $5, $6, NULL, 'pending', '', 0, $7)
ON CONFLICT (source_id, rel_path) DO UPDATE SET
    title        = EXCLUDED.title,
    content_hash = EXCLUDED.content_hash,
    byte_size    = EXCLUDED.byte_size,
    content      = EXCLUDED.content,
    status       = 'pending',
    error_detail = '',
    updated_at   = CURRENT_TIMESTAMP
RETURNING *;

-- name: UpdateInlineDocumentContent :one
UPDATE kb_documents
SET content      = $2,
    content_hash = $3,
    byte_size    = $4,
    status       = 'pending',
    error_detail = '',
    updated_at   = CURRENT_TIMESTAMP
WHERE id = $1 AND origin = 'inline'
RETURNING *;

-- name: SetDocumentTitle :exec
UPDATE kb_documents
SET title = $2, updated_at = CURRENT_TIMESTAMP
WHERE id = $1;

-- name: CountChunksAtGeneration :one
SELECT COUNT(*) FROM kb_chunks
WHERE document_id = $1 AND generation = $2;

-- name: ListDocumentHeadingPaths :many
SELECT DISTINCT heading_path FROM kb_chunks
WHERE document_id = $1 AND generation = $2
ORDER BY heading_path;

-- name: MarkDocumentStatus :exec
UPDATE kb_documents
SET status       = $2,
    error_detail = $3,
    updated_at   = CURRENT_TIMESTAMP
WHERE id = $1;

-- name: MarkDocumentPendingIfCurrent :exec
UPDATE kb_documents
SET status       = 'pending',
    error_detail = $2,
    updated_at   = CURRENT_TIMESTAMP
WHERE id = $1 AND generation < $3;

-- name: AdvanceDocumentGeneration :one
UPDATE kb_documents
SET generation   = $2,
    status       = 'indexed',
    error_detail = '',
    updated_at   = CURRENT_TIMESTAMP
WHERE id = $1 AND generation < $2
RETURNING *;

-- name: DeleteDocumentByID :exec
DELETE FROM kb_documents WHERE id = $1;

-- name: DeleteAbsentFileDocuments :execrows
DELETE FROM kb_documents
WHERE source_id = $1
  AND origin = 'file'
  AND NOT (rel_path = ANY($2::text[]));

-- name: InsertChunk :one
INSERT INTO kb_chunks (
    document_id, owner_id, source_id, generation, ordinal,
    heading_path, content, embed_text, content_hash, embedding,
    embedding_model, token_count, locator
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NULL, $10, $11, $12)
RETURNING id;

-- name: ClaimUnembeddedChunks :many
SELECT
    id, document_id, owner_id, source_id, generation, ordinal,
    heading_path, content, embed_text, content_hash, embedding_model,
    token_count, locator, created_at
FROM kb_chunks
WHERE document_id = $1 AND generation = $2 AND embedding IS NULL
ORDER BY ordinal
LIMIT $3
FOR UPDATE SKIP LOCKED;

-- name: SetChunkEmbedding :exec
UPDATE kb_chunks SET embedding = $2, embedding_model = $3 WHERE id = $1;

-- name: CountUnembeddedChunks :one
SELECT COUNT(*) FROM kb_chunks
WHERE document_id = $1 AND generation = $2 AND embedding IS NULL;

-- name: DeletePendingChunks :exec
DELETE FROM kb_chunks WHERE document_id = $1 AND generation > $2;

-- name: DeleteSupersededChunks :exec
DELETE FROM kb_chunks WHERE document_id = $1 AND generation < $2;

-- name: DeleteChunksByDocument :exec
DELETE FROM kb_chunks WHERE document_id = $1;

-- name: RetrieveKBPassages :many
SELECT
    c.id,
    c.document_id,
    c.source_id,
    s.name AS source_name,
    d.rel_path,
    c.heading_path,
    c.content,
    c.locator,
    (1 - (c.embedding <=> sqlc.arg(embedding)))::float8 AS score
FROM kb_chunks c
JOIN kb_documents d ON d.id = c.document_id
JOIN kb_sources s ON s.id = c.source_id
WHERE c.embedding IS NOT NULL
  AND c.generation = d.generation
  AND c.embedding_model = sqlc.arg(embedding_model)::text
  AND d.generation > 0
  AND s.enabled
  AND c.owner_id = sqlc.arg(owner_id)::bigint
  AND d.owner_id = sqlc.arg(owner_id)::bigint
  AND s.owner_id = sqlc.arg(owner_id)::bigint
  AND d.owner_id = c.owner_id
  AND d.source_id = c.source_id
  AND s.owner_id = c.owner_id
  AND (sqlc.narg(source_id)::bigint IS NULL OR c.source_id = sqlc.narg(source_id)::bigint)
  AND (sqlc.narg(path_prefix)::text IS NULL OR starts_with(d.rel_path, sqlc.narg(path_prefix)::text))
ORDER BY c.embedding <=> sqlc.arg(embedding), c.id
LIMIT sqlc.arg(limit_count)::int;
