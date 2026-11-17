# Spec Delta

## MODIFIED Requirements

### Requirement: A knowledge outline describes the corpus to the caller

The system SHALL expose a read model describing the caller's knowledge corpus: for each of the caller's enabled sources, its identifier, display name, document count, chunk count, and the set of its top-level directory names. The outline SHALL contain no document content and no path outside the source's own relative naming, and SHALL list no other user's source.

#### Scenario: Outline lists the visible sources

- **WHEN** the outline is requested
- **THEN** it lists the caller's own sources with their counts, and no other user's source

#### Scenario: Outline exposes top-level directories

- **WHEN** a source's documents live under `B-Cook/`, `B-Sport/` and `W-Invest/`
- **THEN** the outline lists those three names for that source

#### Scenario: Inline documents contribute no directory names

- **WHEN** an inline source holds only application-created documents
- **THEN** the outline lists that source with its counts and no directory names

#### Scenario: Outline carries no content

- **WHEN** the outline is requested
- **THEN** the response contains no document body text and no absolute filesystem path

## ADDED Requirements

### Requirement: Knowledge retrieval is scoped to the caller's own sources

A knowledge retrieval SHALL return only passages from the caller's own sources. A passage from another user's source SHALL never be returned, and neither the presence nor the absence of such a passage SHALL be inferable from the response. The scope SHALL be enforced on the chunk row, re-asserted against the joined document row, and re-asserted against the joined source row, so a desynchronised ownership column cannot widen the result.

#### Scenario: Another user's source never surfaces

- **WHEN** user B retrieves knowledge and user A has a private source
- **THEN** no passage from user A's private source is returned

#### Scenario: A desynchronised ownership column cannot widen the result

- **WHEN** a chunk row claims one owner while its joined document or source row is owned by another user
- **THEN** the passage is excluded from the result

## REMOVED Requirements

### Requirement: Knowledge retrieval is scoped to the caller's visible corpus

**Reason**: The shared corpus is removed; there is no corpus visible to a caller beyond the caller's own sources.

**Migration**: Replaced by "Knowledge retrieval is scoped to the caller's own sources", which keeps the owner isolation and adds the source-row ownership re-assertion.

### Requirement: A caller in a different embedding space receives nothing from the shared corpus

**Reason**: All chunks now belong to a user's own source, and "Every chunk records its embedding identity" already excludes chunks whose recorded identity differs from the caller's query identity.

**Migration**: No caller-facing behavior is lost beyond the shared-corpus removal; users re-index their own sources with their current embedding configuration.
