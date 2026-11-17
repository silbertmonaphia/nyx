# Spec Delta

## MODIFIED Requirements

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

## ADDED Requirements

### Requirement: A source records its owner

Every knowledge source SHALL record an owner, a display name, a kind, and an enabled flag. A source SHALL belong to exactly one user, and its content SHALL be readable only by that owner.

#### Scenario: A user-owned source is readable only by its owner

- **WHEN** user A owns a source and user B retrieves knowledge
- **THEN** user B's retrieval contains no document from user A's source, while user A's retrieval does

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

## REMOVED Requirements

### Requirement: A source records its scope and owner

**Reason**: Every knowledge source is now private to exactly one user; the deployment-wide `shared` scope is removed.

**Migration**: The migration deletes shared sources with their documents and chunks, drops the `scope` columns, and makes `owner_id` mandatory. Existing per-user inline sources keep their owner; operators re-register former shared directories per user with `kb index --user`.

### Requirement: A caller can remove a source they own

**Reason**: This requirement existed to carve out the operator-managed shared source. The shared source is gone and every remaining source is caller-owned.

**Migration**: Replaced by "A caller can remove any source they own"; no caller-facing capability is lost.
