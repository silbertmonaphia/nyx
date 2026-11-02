# kb-ingestion Specification

## Purpose
The pipeline that turns a file on disk into retrievable passages: normalize the bytes into a heading-aware block sequence, split that sequence into chunks that respect the document's own structure, and give every chunk a vector. The pipeline is change-driven (a content hash decides whether work is needed) and separates parsing from embedding so a failed embedding call never forces a re-parse.

## Requirements

### Requirement: A document is normalized into an ordered block sequence

A Markdown document SHALL be normalized into an ordered sequence of blocks, each carrying the heading path in effect at that position, a kind, and a locator identifying where the block came from. Headings SHALL be recognised at every level and SHALL establish the heading path for the blocks that follow them.

#### Scenario: Headings establish a nested path

- **WHEN** a document contains a level-one heading followed by a level-two heading and a paragraph under it
- **THEN** the paragraph's heading path names both headings, in document order

### Requirement: Block kinds preserve code, tables, and front matter

Fenced code blocks SHALL be preserved intact as single blocks. Tables SHALL be preserved as table blocks rather than flattened into prose. Front matter SHALL not appear in any block's text.

#### Scenario: Code block stays intact

- **WHEN** a document contains a fenced code block of twelve lines
- **THEN** the code block appears as one block whose text contains all twelve lines

#### Scenario: Table stays a table block

- **WHEN** a document contains a Markdown table
- **THEN** the table appears as a single table block rather than as separate cell blocks

#### Scenario: Front matter is excluded

- **WHEN** a document begins with front matter followed by a heading
- **THEN** no block's text contains the front matter, and the first block belongs to the first heading

### Requirement: Image references contribute only their alt text

An image reference SHALL contribute its alt text, if any, as ordinary text in the block in effect, and SHALL never contribute the image URL. An image reference with no alt text SHALL contribute nothing. v1 does not parse image content itself.

#### Scenario: An image with alt text contributes only the alt text

- **WHEN** a document contains `![黄昏的庆云顶](https://example.com/img.jpg)` as its own line
- **THEN** a block carries the text `黄昏的庆云顶`, and no block's text contains the URL

#### Scenario: An image without alt text contributes nothing

- **WHEN** a document contains `![](https://example.com/img.jpg)` as its own line
- **THEN** no block carries the URL, and the line does not produce an empty text block

### Requirement: A chunk never spans two heading paths

A chunk SHALL belong to exactly one heading path and SHALL NOT span two heading paths. A section below the configured minimum size SHALL be kept rather than dropped and SHALL still carry its heading.

#### Scenario: Chunk never spans two headings

- **WHEN** a document has a paragraph under one heading followed by a paragraph under a different heading
- **THEN** no chunk contains both paragraphs

#### Scenario: Short section survives with its heading

- **WHEN** a section contains only a single short line
- **THEN** it produces a chunk whose embedded text includes that section's heading path

### Requirement: Atomic blocks and the token cap

No chunk's embedded text SHALL exceed the configured hard token cap, except a fenced code block kept whole. A table SHALL NOT be split across chunks; one larger than the cap SHALL be split by rows with its header repeated in each part. A fenced code block SHALL NOT be split, and one larger than the hard cap SHALL still be kept whole.

#### Scenario: Oversized table splits by rows with a repeated header

- **WHEN** a table exceeds the hard token cap
- **THEN** it is split into multiple chunks, each of which begins with the table's header row

#### Scenario: An oversized fenced code block is kept whole

- **WHEN** a document contains a fenced code block larger than the hard token cap
- **THEN** it produces one chunk that may exceed the cap, and no content from another heading path joins that chunk

### Requirement: Embedded text carries a context header

The text that is embedded for a chunk SHALL be the chunk's relative path, its heading path, and its body, in that order. The body alone SHALL be stored separately for citation.

#### Scenario: Embedded text carries the provenance header

- **WHEN** a chunk is produced for a file at `B-Cook/2019-11-16-Cook-Diet.md` under the heading path `煮食记 / 碳水列表`
- **THEN** the chunk's embedded text begins with that relative path and heading path, and the separately stored body contains neither

### Requirement: A fence too large for the embedding window leaves the document pending

A fenced code block whose size exceeds the embedding model's usable window cannot receive an embedding. The document SHALL then end pending with that reason observable, the previous generation SHALL continue to serve retrieval when one exists, and the pipeline SHALL NOT truncate code to fit.

#### Scenario: A fence too large for the embedding window leaves the document pending

- **WHEN** a fenced code block kept whole exceeds the embedding model's usable window and no embedding can be produced for it
- **THEN** the document ends pending with the reason recorded, any previously indexed generation of that document remains retrievable, and no code text is truncated to fit

### Requirement: Indexing skips unchanged documents

Each document SHALL carry a hash of the content it was indexed from — the raw file bytes for a directory document, the stored body for an inline document. When a scan finds a document whose hash equals the stored hash, the document SHALL be skipped entirely and no embedding call SHALL be made for it.

#### Scenario: Unchanged document costs no embedding call

- **WHEN** a scan runs twice over an unmodified document
- **THEN** the second scan performs no embedding call for that document and its chunks are unchanged

### Requirement: Changed and removed documents are re-indexed

When the hash differs, the document's existing chunks SHALL be removed and it SHALL be re-chunked and re-embedded. When a previously indexed directory document is no longer present and the scan found at least one readable candidate, its chunks SHALL be removed and it SHALL no longer be retrievable. A scan that finds no readable candidate at all SHALL leave existing documents in place, because an empty or unreadable root may be a transient mount failure.

#### Scenario: Changed document is re-indexed

- **WHEN** the bytes of an indexed directory document change
- **THEN** its previous chunks are removed and new chunks are produced for the new content

#### Scenario: Removed document stops being retrievable

- **WHEN** a document that was indexed is deleted from its source directory and the scan still finds another readable candidate
- **THEN** a later scan removes its chunks and a retrieval no longer returns it

#### Scenario: An empty or unreadable root preserves documents

- **WHEN** a scan finds no readable candidate files, for example because the root is unreadable or its mount is temporarily empty
- **THEN** existing documents and chunks are left untouched and the scan reports the failure when the root itself is unreadable

### Requirement: A file document is authoritative on disk

Every document SHALL carry an origin of `file` (authoritative content is a file on disk) or `inline` (authoritative content is stored with the document). A document's origin SHALL NOT change once it exists. Editing a `file` document through the application SHALL be refused with a static message that names no path and SHALL leave the document and its chunks untouched.

#### Scenario: Editing a directory document in place is refused

- **WHEN** a caller attempts to edit the content of a document whose origin is `file`
- **THEN** the edit is refused with a static message that names no path, and the document's content and chunks are unchanged

### Requirement: An inline document is authoritative in the database

Editing an `inline` document SHALL replace its authoritative content, re-chunk and re-embed it, and keep the same document identifier, source, and relative name.

#### Scenario: Editing an inline document re-indexes it in place

- **WHEN** a caller edits the content of a document whose origin is `inline`
- **THEN** the document keeps its identifier, its stored content becomes the new body, its chunks are replaced by chunks of the new body, and the new chunks are retrievable

#### Scenario: An inline document that fails to re-index keeps its previous chunks

- **WHEN** an inline document is edited and the embedding call fails
- **THEN** the document's stored content is the new body, the document is pending, and the retrieval still returns its previous chunks rather than none

### Requirement: Parsing is separated from embedding, and the embedding pass is resumable

Chunks SHALL be written with an empty embedding before any embedding call is made. A later fill pass SHALL claim unembedded chunks and store their vectors. A failed embedding call SHALL leave the document pending with its chunks already parsed, and a subsequent fill pass SHALL complete it without re-parsing the source file. Chunks with no embedding SHALL never be returned by a retrieval.

#### Scenario: Embedding failure leaves parsed chunks in place

- **WHEN** the embedding endpoint fails during the indexing of a document
- **THEN** the new chunks are stored with no embedding, the document is pending, and the source file is not parsed again by a later catch-up

#### Scenario: Fill pass completes the pending document

- **WHEN** a fill pass runs while the embedding endpoint is healthy
- **THEN** the pending document's new chunks receive embeddings, the document becomes indexed, and the new chunks replace the previous ones as the retrievable content

#### Scenario: Unembedded chunks are never retrieved

- **WHEN** a chunk has no embedding
- **THEN** a retrieval returns no such chunk

### Requirement: A document swaps generations only when fully embedded

A document SHALL advance to its newly indexed content only when the new chunks are fully embedded. Until then its previously indexed content SHALL remain the retrievable one.

#### Scenario: A document stays retrievable while its replacement is pending

- **WHEN** a document's content changed and the replacement chunks have not been embedded yet
- **THEN** a retrieval returns the document's previously indexed chunks, not none

### Requirement: Indexing runs on an explicit trigger and on a bounded lazy catch-up

Indexing SHALL run when the caller explicitly triggers it over the sources visible to that caller. Indexing SHALL ALSO run as a background catch-up when a chat request observes documents that are still pending or chunks that are still unembedded. The catch-up SHALL run on a detached context, SHALL be bounded in the work it performs per invocation, and SHALL NOT delay or fail the chat request that triggered it.

#### Scenario: Explicit trigger indexes the caller's visible sources

- **WHEN** a user triggers indexing
- **THEN** the caller's private sources and the shared source are scanned and their changed documents are indexed

#### Scenario: Chat triggers a bounded catch-up

- **WHEN** a chat request observes a pending document
- **THEN** a background catch-up begins on a detached context and the chat request completes without waiting for it

#### Scenario: Catch-up does not fail the chat

- **WHEN** the catch-up fails
- **THEN** the chat request still succeeds and the failure is recorded on the source

### Requirement: Every document has an observable status

Each document SHALL carry a status of `pending`, `indexed`, or `failed`, together with a reason when it is `failed`. A document contributes only chunks of its current generation; a document that has never completed a generation SHALL contribute no chunk to a retrieval.

#### Scenario: Failed document is recorded with a reason

- **WHEN** a document cannot be parsed
- **THEN** the document's status is `failed` and it carries a reason naming the failure

#### Scenario: A document without a completed generation is not retrievable

- **WHEN** a retrieval runs over a corpus containing a pending or failed document that has never advanced a generation
- **THEN** no chunk from that document is returned

#### Scenario: A previously indexed document keeps serving while pending

- **WHEN** a document's replacement content is indexed but not yet fully embedded
- **THEN** a retrieval returns its previous generation's chunks, not none

### Requirement: A document's origin decides whether it can be deleted in place

Deleting an inline document SHALL remove the document and every chunk of it, and it SHALL stop being retrievable. Deleting a document whose origin is `file` SHALL be refused with a static message that names no path, and SHALL leave the document and its chunks untouched — the file on disk is the truth, so the file is what has to change. A refused deletion SHALL NOT be a silent no-op: the caller SHALL be told the document is managed by a file.

#### Scenario: Deleting an inline document removes it

- **WHEN** a caller deletes an inline document
- **THEN** the document and its chunks are gone and a retrieval returns nothing from it

#### Scenario: Deleting a file-backed document is refused

- **WHEN** a caller attempts to delete a document whose origin is `file`
- **THEN** the deletion is refused with a static message that names no path, and the document's content and chunks are unchanged

### Requirement: Only Markdown content is ingested

The ingestion pipeline SHALL ingest Markdown documents. A file of any other type SHALL be skipped with a recorded reason rather than causing the scan to fail, and SHALL contribute no document and no chunk.

#### Scenario: Non-Markdown file is skipped, not fatal

- **WHEN** a source directory contains a file that is not Markdown
- **THEN** the scan continues, the file is skipped with a recorded reason, and no document is created for it

#### Scenario: An unsupported file type does not stop the scan

- **WHEN** a scan encounters an unsupported file in the middle of a directory tree
- **THEN** the remaining files in that tree are still scanned and indexed
