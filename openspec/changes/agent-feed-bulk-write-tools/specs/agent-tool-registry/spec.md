# Spec Delta

## ADDED Requirements

### Requirement: Bulk write tools select targets by caller-supplied query

The registry SHALL expose mutating tools that update or soft-delete **every** feed matching a caller-supplied query, alongside the existing single-target-by-id tools. A bulk write tool's target selection SHALL be resolved server-side from the query, never from an id list the caller supplies and never from a model-chosen subset. Its arguments SHALL include the query (required, non-empty, length-bounded), and the existing per-owner scoping SHALL apply unchanged: the match is computed over the authenticated caller's own feeds only, and a query argument MUST NOT widen that scope. Where the caller needs a single feed, the existing by-id tools remain the correct choice and continue to be registered.

#### Scenario: Plural mutation reaches every matching feed

- **WHEN** the caller invokes the bulk update tool with a query matching three of the caller's feeds
- **THEN** all three feeds are updated, and the response identifies each one

#### Scenario: Selection stays inside the caller's feeds

- **WHEN** a caller invokes a bulk write tool with a query that textually matches feeds owned by another user
- **THEN** only the caller's own matching feeds are considered; no other user's feed is read, counted, or mutated, and the response does not reveal their existence

#### Scenario: Single-target tools are unaffected

- **WHEN** the caller invokes the existing `update_feed` or `delete_feed` by id
- **THEN** that feed alone is affected, and the bulk tools are not consulted

#### Scenario: Empty match is a no-op result, not an error

- **WHEN** a bulk write tool's query matches none of the caller's feeds
- **THEN** the call returns a successful result reporting zero affected targets, and no row is written or deleted

### Requirement: A bulk write refuses an unbounded match

A bulk write tool SHALL take a configured maximum match size. When the query matches more feeds than that maximum, the tool MUST refuse the whole mutation and report that the match exceeded the limit, rather than mutating a prefix of the matched set. Refusing is per-call, not per-run: a subsequent call with a narrower query proceeds normally. The refusal MUST NOT be classified as a correctable-argument or transient failure that triggers an automatic retry of the same call.

#### Scenario: Over-broad query is refused whole

- **WHEN** a delete query matches more feeds than the configured maximum
- **THEN** no feed is deleted, the result reports the match exceeded the limit, and the failure is not retried automatically

#### Scenario: Narrower query proceeds

- **WHEN** the same caller retries with a narrower query that matches within the maximum
- **THEN** the mutation executes normally

### Requirement: Bulk write returns the exact per-target outcome

A bulk write tool's result SHALL identify every target it acted on and every target it did not, with a per-target reason for each failure. A partial outcome MUST be reported as partial rather than collapsed into an unqualified success, and the result SHALL be the source the answering step reads when it tells the user what happened. An internal error string MUST NOT appear in that reason; failures pass through the safe-error funnel like every other tool error.

#### Scenario: Partial failure is visible in the result

- **WHEN** a bulk update matches five feeds and one of them cannot be updated
- **THEN** the result names the four updated targets and the one failed target with its reason, rather than reporting an unqualified success

#### Scenario: Result carries no internal error text

- **WHEN** a per-target failure originates from an internal error
- **THEN** the reason in the result is the safe, mapped detail and the internal error text does not reach the caller