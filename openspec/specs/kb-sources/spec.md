# kb-sources Specification

## Purpose
The registry of where knowledge content comes from and who may see it. A source is one place content is read from — a per-user scanned directory registered by an operator, or a per-user inline store of application-created documents — together with the ownership rule that governs everything indexed from it. No document from one user's source is ever visible to another user.

## Requirements

### Requirement: A source records its owner

Every knowledge source SHALL record an owner, a display name, a kind, and an enabled flag. A source SHALL belong to exactly one user, and its content SHALL be readable only by that owner.

#### Scenario: A user-owned source is readable only by its owner

- **WHEN** user A owns a source and user B retrieves knowledge
- **THEN** user B's retrieval contains no document from user A's source, while user A's retrieval does

### Requirement: A source's kind and enabled flag decide its contribution

A source's kind SHALL be `dir` — a scanned directory owned by one user and carrying a root path — or `inline` — application-created documents with no root path. A disabled source SHALL contribute no documents to retrieval and SHALL be skipped by scans. The source list SHALL still report a disabled source so its owner can re-enable it.

#### Scenario: Disabled source contributes nothing

- **WHEN** a source is disabled
- **THEN** a scan skips it and a retrieval returns no document that was indexed from it

#### Scenario: A disabled source remains listed

- **WHEN** a caller's source is disabled
- **THEN** it remains in the caller's source list so it can be re-enabled

#### Scenario: An inline source has no root path

- **WHEN** a user's inline source is listed
- **THEN** it carries no root path and no directory scan is scheduled for it

### Requirement: A directory source is registered for one user by the operator

A directory source SHALL be created for exactly one user by an operator action that names the user, an existing readable directory, and a source name. Registering a name that already exists for that user SHALL repoint that user's directory source to the new directory, and SHALL NOT convert that user's inline source. A directory source SHALL NOT be creatable through the caller-facing API. Source names SHALL be unique per user, and the inline source name SHALL be reserved.

#### Scenario: The directory is indexed into the user's own corpus

- **WHEN** an operator registers a directory for user A and indexing runs
- **THEN** the directory's documents are indexed into a source owned by user A, and user B's retrieval contains none of them

#### Scenario: Re-registering the same name repoints the directory

- **WHEN** an operator registers a different directory under a source name that already exists for the same user
- **THEN** that user's existing directory source points at the new directory and the next scan indexes it

#### Scenario: The inline source cannot be converted

- **WHEN** a directory registration names the reserved inline source name
- **THEN** the registration is refused and the inline source and its documents are unchanged

### Requirement: A source reports its freshness and content counts

The source list SHALL report, per source, the time of its last completed scan, the number of documents indexed from it, the number of chunks indexed from it, the number of documents still awaiting indexing, and the reason for the most recent failure when one exists. A source that has never been scanned SHALL report zero counts and no scan time.

#### Scenario: Counts reflect the last scan

- **WHEN** a source's scan indexes twelve documents producing forty chunks
- **THEN** the source reports twelve documents, forty chunks, zero pending, and the scan time

#### Scenario: Unscanned source reports zero

- **WHEN** a source has never been scanned
- **THEN** it reports zero documents, zero chunks, zero pending, and no scan time

### Requirement: An upload is content-sniffed

An upload SHALL be accepted only after its bytes are confirmed to be text or Markdown content; the client's file extension and declared content type SHALL NOT be sufficient.

#### Scenario: Non-text upload is refused

- **WHEN** a user uploads a file whose bytes are not text or Markdown
- **THEN** the upload is refused, no document is registered, and the body is not retained

### Requirement: An uploaded document is stored inline and indexed

The accepted body SHALL be stored as the authoritative content of an inline document owned by the uploading user, in that user's inline source, and indexed through the same pipeline that indexes directory documents. The upload SHALL NOT be written to the filesystem, and no upload sink path SHALL be configured. A document MAY also be created directly with a body typed into the application, with the same ownership, storage, and indexing behaviour.

#### Scenario: Upload registers and indexes an inline document

- **WHEN** a user uploads a Markdown file
- **THEN** a document owned by that user is registered with the uploaded body as its authoritative content, and its chunks become retrievable

#### Scenario: A document created in the application is stored the same way

- **WHEN** a user creates a document with a body typed into the application
- **THEN** it is registered as an inline document owned by that user with that body as its authoritative content, and its chunks become retrievable

### Requirement: Identical inline content is stored once

Uploading content whose hash already matches an inline document the user owns SHALL leave that document's chunks in place and SHALL NOT create a second copy.

#### Scenario: Re-upload of identical content is not double-indexed

- **WHEN** a user uploads content whose hash matches an inline document that user already owns
- **THEN** the existing document's chunks are left in place and no second copy of its chunks is created

### Requirement: Uploaded content is private to its owner

An inline document SHALL be readable only by the user who owns it.

#### Scenario: Uploaded content is not visible to another user

- **WHEN** user A uploads a document and user B retrieves knowledge
- **THEN** user B's retrieval contains no chunk from user A's uploaded document

### Requirement: A caller can remove any source they own

A caller SHALL be able to remove one of their own sources. Removing a source SHALL remove every document indexed from it and every chunk of those documents, and the source SHALL stop appearing in the caller's source list. A source owned by another user SHALL NOT be removable through the caller-facing API.

#### Scenario: Removing a user-owned source removes its documents

- **WHEN** a caller removes their user-scope source
- **THEN** the source, its documents and their chunks are gone, and a retrieval returns nothing from them

#### Scenario: Removing an inline source removes its documents

- **WHEN** a caller removes their inline source
- **THEN** the source, its inline documents and their chunks are gone

#### Scenario: Another user's source is not removable

- **WHEN** a caller attempts to remove a source owned by another user
- **THEN** the request is refused and the source and its documents remain
