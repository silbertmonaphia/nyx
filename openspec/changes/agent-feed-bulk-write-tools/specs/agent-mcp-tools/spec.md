# Spec Delta

## ADDED Requirements

### Requirement: Bulk mutation confirmation shows its scope before the decision

When a confirmation is required for a tool that can affect more than one record, the emitted `confirm_request` frame SHALL carry the number of records the call would affect and a bounded sample of their identifying details (for feeds, their titles), so the user is not asked to approve an invisible blast radius. The count and sample SHALL be computed from the same owner-scoped query the call would execute, and MUST NOT reveal records outside the caller's own scope. A single-record call keeps its existing confirmation frame unchanged.

#### Scenario: Bulk delete shows count and titles before approval

- **WHEN** a delete-by-query step is paused for confirmation and the query matches the caller's three feeds
- **THEN** the `confirm_request` frame states that three feeds would be soft-deleted and names them (bounded to a documented sample size), alongside the tool name, arguments, approval id, and expiry

#### Scenario: A single-record call is unchanged

- **WHEN** `delete_feed` by id is paused for confirmation
- **THEN** the frame carries the same fields it does today, with no count or sample added

#### Scenario: The preview does not leak other users' records

- **WHEN** the confirm-request scope preview is computed for a query that textually matches another user's feeds
- **THEN** the count and sample cover only the caller's own matching feeds