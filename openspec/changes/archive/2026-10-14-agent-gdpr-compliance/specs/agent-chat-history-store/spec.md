# Spec Delta

## Purpose

(see existing capability)

## ADDED Requirements

### Requirement: Owner erasure is independent of retention state

The storage layer SHALL expose owner-scoped erase helpers used by the GDPR path (see `agent-gdpr-erasure`): deleting all of a user's sessions and messages MUST remove soft-deleted rows as well (retention phase-1 is not a precondition — an erasure request deletes whatever exists), and MUST filter by `user_id` in every statement (never delete-by-session-id without the owner predicate). The helpers SHALL return per-table counts so the erase response and audit summary can report them.

#### Scenario: Erase removes soft-deleted rows too

- **WHEN** a user's data includes sessions in state `deleted_at IS NOT NULL`
- **THEN** the owner-erase helper deletes those sessions and their messages along with the live ones

#### Scenario: Erase is owner-scoped in SQL

- **WHEN** two users each have sessions and one user erases
- **THEN** only the erasing user's rows are removed; the helper's statements all carry the `user_id` predicate (pinned by a test with a foreign user's rows present)
