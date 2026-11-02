# kb-retrieval Specification

## Purpose
Retrieval over the knowledge corpus: which passages a caller may see, how a query is narrowed, how the knowledge corpus and the feed corpus are combined, and how a returned passage is attributed to the file it came from. The feed corpus keeps its existing behaviour; everything here adds a second corpus beside it.

## Requirements

### Requirement: Knowledge retrieval is scoped to the caller's visible corpus

A knowledge retrieval SHALL return only passages from the shared source and from the caller's own private sources. A passage from another user's private source SHALL never be returned, and neither the presence nor the absence of such a passage SHALL be inferable from the response. The scope SHALL be enforced on the chunk row AND re-asserted against the joined document row, so a desynchronised ownership column cannot widen the result.

#### Scenario: Another user's private source never surfaces

- **WHEN** user B retrieves knowledge and user A has a private source
- **THEN** no passage from user A's private source is returned

#### Scenario: Shared passages are returned to every caller

- **WHEN** a shared source is indexed and any user retrieves knowledge
- **THEN** passages from that shared source are eligible to be returned

#### Scenario: A desynchronised ownership column cannot widen the result

- **WHEN** a chunk row claims shared scope while its joined document row is owned by a user
- **THEN** the passage is excluded from the result

### Requirement: Retrieval can be narrowed by source and by path prefix

A knowledge retrieval SHALL accept an optional source filter and an optional relative-path prefix filter. When a source filter is given, only passages from that source SHALL be returned. When a path prefix is given, only passages whose document path lies under that prefix SHALL be returned. When neither is given, the whole visible corpus SHALL be searched.

#### Scenario: Source filter narrows the corpus

- **WHEN** a retrieval carries a source filter naming one source
- **THEN** no passage from any other source is returned

#### Scenario: Path prefix filter narrows within a source

- **WHEN** a retrieval carries the path prefix `B-Sport/`
- **THEN** only passages whose document path lies under `B-Sport/` are returned

#### Scenario: Filters are optional

- **WHEN** a retrieval carries neither filter
- **THEN** passages from every visible source are eligible to be returned

### Requirement: Corpus selection is explicit

A retrieval SHALL name the corpus it searches: `feeds`, `kb`, or `both`. A `kb` retrieval SHALL search only the knowledge corpus.

#### Scenario: KB-only retrieval ignores the feed corpus

- **WHEN** a retrieval names the `kb` corpus
- **THEN** no feed passage is returned, even when feed passages would have scored highly

### Requirement: The feed corpus is unaffected

A `feeds` retrieval SHALL return exactly the passages and order the feed corpus returned before this change; its citations gain the additive `kind` discriminator required by the bridge delta.

#### Scenario: Feeds-only retrieval is unchanged

- **WHEN** a retrieval names the `feeds` corpus
- **THEN** the returned passages and their order match the pre-existing feed retrieval for the same query

### Requirement: Both-corpus fusion is by rank

A `both` retrieval SHALL take the top results from each corpus and combine them by rank rather than by score, so a result's position within its own corpus — not its raw similarity number — decides the combined order.

#### Scenario: Both-corpus retrieval fuses by rank

- **WHEN** a retrieval names the `both` corpus and the two corpora return passages with different similarity scores
- **THEN** the combined order follows each passage's rank within its own corpus rather than its raw score

### Requirement: A returned knowledge passage carries its provenance

Each returned knowledge passage SHALL carry the document it came from, the source it belongs to, the document's relative path, its heading path, and a locator identifying the region of the file it came from. The passage's quoted text SHALL be the chunk body, not the embedded text, so the provenance header does not appear inside the quoted excerpt.

#### Scenario: Passage names its file and section

- **WHEN** a knowledge passage is returned
- **THEN** it carries the source name, the relative path, the heading path, and a locator

#### Scenario: The quoted text excludes the provenance header

- **WHEN** a knowledge passage is rendered for a model or a user
- **THEN** the rendered excerpt is the chunk body alone and does not contain the embedded path-and-heading prefix

### Requirement: Every chunk records its embedding identity

Every indexed chunk SHALL record the identity of the embedding model that produced its vector. A retrieval SHALL return only chunks whose recorded identity matches the identity that produced the caller's query vector, regardless of which source the chunk came from.

#### Scenario: The filter is on the recorded identity, not the source

- **WHEN** a chunk's recorded embedding identity differs from the caller's
- **THEN** it is excluded regardless of which source it came from

#### Scenario: A caller's own passages are unaffected

- **WHEN** a user's private corpus was indexed under the same embedding model the caller queries with
- **THEN** those passages remain retrievable

### Requirement: A caller in a different embedding space receives nothing from the shared corpus

A shared corpus is indexed once under the deployment's embedding identity. A caller whose own embedding identity differs SHALL not receive its passages, and no shared passage's score SHALL be computed against the caller's vector.

#### Scenario: A caller in a different embedding space receives nothing from the shared corpus

- **WHEN** the shared corpus was indexed under embedding model A and the caller queries with a vector produced by embedding model B
- **THEN** no shared passage is returned, and no shared passage's score is computed against the caller's vector

### Requirement: A knowledge outline describes the corpus to the caller

The system SHALL expose a read model describing the visible knowledge corpus: for each visible source, its identifier, display name, scope, document count, chunk count, and the set of its top-level directory names. The outline SHALL contain no document content and no path outside the source's own relative naming.

#### Scenario: Outline lists the visible sources

- **WHEN** the outline is requested
- **THEN** it lists the shared source and the caller's own private sources with their counts, and no other user's private source

#### Scenario: Outline exposes top-level directories

- **WHEN** a source's documents live under `B-Cook/`, `B-Sport/` and `W-Invest/`
- **THEN** the outline lists those three names for that source

#### Scenario: Inline documents contribute no directory names

- **WHEN** an inline source holds only application-created documents
- **THEN** the outline lists that source with its counts and no directory names

#### Scenario: Outline carries no content

- **WHEN** the outline is requested
- **THEN** the response contains no document body text and no absolute filesystem path
