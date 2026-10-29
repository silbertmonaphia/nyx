# Spec Delta

## Purpose

The registry of where knowledge content comes from and who may see it. A source is one place content is read from — an operator-managed shared directory, a per-user private directory, or a per-user inline store of application-created documents — together with the visibility rule that governs everything indexed from it. The registry is the trust boundary for path handling: a user's private root is operator-confined, and no document from one user's private source is ever visible to another.

## ADDED Requirements

### Requirement: A source declares its origin, its owner, and its visibility

Every knowledge source SHALL record a scope, an owner, a display name, a kind, and an enabled flag. A source's scope SHALL be either `shared` — readable by every user of the deployment — or `user`, in which case an owner is recorded and the source's content is readable only by that owner. A source's kind SHALL be either `dir` — a directory tree that is scanned, carrying a root path — or `inline` — a store of application-created documents that carries no root path. A disabled source SHALL contribute no documents to retrieval and SHALL be skipped by scans.

#### Scenario: Shared source is readable by every user

- **WHEN** a deployment configures one shared source and two different users retrieve knowledge
- **THEN** both users' retrievals include documents indexed from that shared source

#### Scenario: Private source is readable only by its owner

- **WHEN** user A registers a private source and user B retrieves knowledge
- **THEN** user B's retrieval contains no document from user A's private source, while user A's retrieval does

#### Scenario: Disabled source contributes nothing

- **WHEN** a source is disabled
- **THEN** a scan skips it and a retrieval returns no document that was indexed from it

#### Scenario: An inline source has no root path

- **WHEN** a user's inline source is listed
- **THEN** it carries no root path and no directory scan is scheduled for it

### Requirement: A private root is confined to an operator-configured base

A user-supplied private root SHALL be accepted only when its fully resolved real path lies inside the operator-configured private base directory. The check SHALL be applied when the root is saved AND again when the source is scanned, so a root that is replaced by a symlink after saving is refused at scan time. A root that does not exist, is not a directory, or resolves outside the base SHALL be refused with a static, non-leaking message. The refusal MUST NOT disclose the resolved path or any filesystem detail.

#### Scenario: Root inside the base is accepted

- **WHEN** a user saves a private root that resolves inside the operator-configured private base
- **THEN** the source is stored and appears in the user's source list

#### Scenario: Root outside the base is refused

- **WHEN** a user saves a private root that resolves outside the operator-configured private base
- **THEN** the save is refused with a static message that names no path, and no source is created

#### Scenario: Symlink escape is refused at scan time

- **WHEN** a saved private root is replaced by a symlink that points outside the private base
- **THEN** the scan refuses that root, records the failure on the source, and indexes nothing from it

#### Scenario: Missing root is refused

- **WHEN** a user saves a private root that does not exist or is not a directory
- **THEN** the save is refused and no source is created

### Requirement: A source reports its freshness and content counts

The source list SHALL report, per source, the time of its last completed scan, the number of documents indexed from it, the number of chunks indexed from it, the number of documents still awaiting indexing, and the reason for the most recent failure when one exists. A source that has never been scanned SHALL report zero counts and no scan time.

#### Scenario: Counts reflect the last scan

- **WHEN** a source's scan indexes twelve documents producing forty chunks
- **THEN** the source reports twelve documents, forty chunks, zero pending, and the scan time

#### Scenario: Unscanned source reports zero

- **WHEN** a source has never been scanned
- **THEN** it reports zero documents, zero chunks, zero pending, and no scan time

### Requirement: An uploaded document is stored inline and owned by the uploader

An upload SHALL be accepted only after its bytes are confirmed to be text or Markdown content; the client's file extension and declared content type SHALL NOT be sufficient to accept it. The accepted body SHALL be stored as the authoritative content of an inline document owned by the uploading user, in that user's inline source. The upload SHALL NOT be written to the filesystem as a managed directory, and no upload sink path SHALL be configured. The document SHALL be indexed through the same pipeline that indexes directory documents. Uploading content whose hash already matches an inline document the user owns SHALL leave that document's chunks in place and SHALL NOT create a second copy. A document MAY also be created directly with a body typed into the application, with the same ownership, storage and indexing behaviour as an upload.

#### Scenario: Upload registers and indexes an inline document

- **WHEN** a user uploads a Markdown file
- **THEN** a document owned by that user is registered with the uploaded body as its authoritative content, and its chunks become retrievable

#### Scenario: A document created in the application is stored the same way

- **WHEN** a user creates a document with a body typed into the application
- **THEN** it is registered as an inline document owned by that user with that body as its authoritative content, and its chunks become retrievable

#### Scenario: Non-text upload is refused

- **WHEN** a user uploads a file whose bytes are not text or Markdown
- **THEN** the upload is refused, no document is registered, and the body is not retained

#### Scenario: Re-upload of identical content is not double-indexed

- **WHEN** a user uploads content whose hash matches an inline document that user already owns
- **THEN** the existing document's chunks are left in place and no second copy of its chunks is created

#### Scenario: Uploaded content is not visible to another user

- **WHEN** user A uploads a document and user B retrieves knowledge
- **THEN** user B's retrieval contains no chunk from user A's uploaded document

### Requirement: A caller can remove a source they own

A caller SHALL be able to remove one of their own sources. Removing a source SHALL remove every document indexed from it and every chunk of those documents, and the source SHALL stop appearing in the caller's source list. A caller SHALL NOT be able to remove a shared source through the caller-facing API; the shared source is operator-managed and is removed only through the operator's own tooling.

#### Scenario: Removing a private source removes its documents

- **WHEN** a caller removes their private source
- **THEN** the source, its documents and their chunks are gone, and a retrieval returns nothing from them

#### Scenario: Removing an inline source removes its documents

- **WHEN** a caller removes their inline source
- **THEN** the source, its inline documents and their chunks are gone

#### Scenario: The shared source is not removable by a caller

- **WHEN** a caller attempts to remove the shared source
- **THEN** the request is refused with a static message and the shared source and its documents remain
