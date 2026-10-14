# Spec Delta

## Purpose

Gives the operator an owner-scoped data-export path and a right-to-erasure path covering every table the agent service writes, with an append-only audit trail — the compliance closure for a system that stores chat history (1-year floor) and derived preferences.

## ADDED Requirements

### Requirement: Owner-scoped data export

The service SHALL expose `GET /agent/me/export`, scoped to the user id in the `X-Nyx-User-Id` header (missing/non-integer → 400; no admin override, no cross-user parameter). The response SHALL be one JSON object containing: `user_id`, `exported_at`, `sessions` (every `agent.chat_sessions` row for the user, including soft-deleted ones), `messages` (every `agent.chat_messages` row belonging to those sessions, content included), `prefs` (every `agent.chat_user_prefs` row for the user), `runs` (every `agent.runs` row for the user), and `checkpoint_thread_ids` (the run ids whose langGraph checkpoint threads hold this user's state — ids only; checkpoint content is erased by deleting the thread, so the id list is the complete representation). A user with no data SHALL receive the same object with empty arrays (never 404). Each call SHALL append an `export` row to `agent.data_audit_log` with counts only.

#### Scenario: Export is complete for one user

- **WHEN** a user with 2 sessions, 5 messages, 1 pref, and 2 runs calls the export
- **THEN** the JSON carries all of those rows and exactly the user's two run ids as `checkpoint_thread_ids`
- **AND** none of another user's rows appear anywhere in the body

#### Scenario: Empty export is a well-formed object

- **WHEN** a user with no data calls the export
- **THEN** the response is 200 with empty arrays, not 404 and not an error

#### Scenario: Export is audited with counts, not content

- **WHEN** an export completes
- **THEN** `agent.data_audit_log` gains one row `(user_id, 'export', {"sessions": N, "messages": M, ...}, now())`
- **AND** the audit row contains no message content

### Requirement: Right to erasure across every store

The service SHALL expose `DELETE /agent/me`, scoped to the header user id, physically deleting the user's data in this order: `chat_messages` (all messages of the user's sessions, including sessions already soft-deleted) → `chat_sessions` → `chat_user_prefs` → `agent.runs` (capturing the deleted run ids first) → `langGraph` checkpoint thread deletion via `adelete_thread(run_id)` for every captured run id → append the audit row. The response SHALL be `{"deleted": {"sessions": n1, "messages": n2, "prefs": n3, "runs": n4, "threads": n5}}` where `threads` is the count of `adelete_thread` calls that completed without error (a per-thread failure is warn-logged, counted as not-deleted, and does NOT abort the remaining deletions or the audit row). The operation SHALL be idempotent: a second call deletes nothing and returns zero counts with 200.

#### Scenario: Erase walks every table

- **WHEN** a user with sessions, messages, prefs, runs, and checkpoint threads calls erase
- **THEN** every listed table has zero rows for that user afterward, `adelete_thread` was invoked once per run id, and the response counts match the row counts deleted

#### Scenario: Soft-deleted sessions are erased too

- **WHEN** the user's data includes a session already soft-deleted by the user (or by retention phase 1)
- **THEN** erasure removes it and its messages as well — retention is not a precondition for erasure

#### Scenario: Double erase is safe

- **WHEN** erase runs twice
- **THEN** the second call returns 200 with all-zero counts; no error; a second `erase` audit row is appended

#### Scenario: Checkpoint GC failure is contained

- **WHEN** `adelete_thread` raises for one run id
- **THEN** the remaining run ids are still attempted, the failure is warn-logged, `threads` counts only successes, the audit row is still written, and the response is still 200

#### Scenario: Cross-user isolation

- **WHEN** user A erases
- **THEN** user B's rows in every table are untouched (counts verified before/after)

### Requirement: Append-only audit log with 3-year retention

`agent.data_audit_log(id bigserial, user_id bigint, action text, payload_summary jsonb, requested_at timestamptz)` SHALL be append-only from application code: the only writes are `record()` inserts and the retention cron's deletes. `payload_summary` SHALL carry counts and timestamps only — never message content, preference values, or any copy of the data being audited. The audit retention (`PYTHON_AGENT_AUDIT_RETENTION_DAYS`, default 1095) SHALL be ≥ 365 so the audit trail always outlives the data it audits; rows older than the window SHALL be deleted by a retention iteration (span `agent.audit.retention`, counter `agent_audit_retention_deleted_total`).

#### Scenario: Audit rows outlive data rows

- **WHEN** a chat session is hard-deleted by chat retention (365 days) and its erase/export audit rows are 2 years old
- **THEN** the audit rows still exist (3-year window)

#### Scenario: Retention reaps old audit rows

- **WHEN** an audit row's `requested_at` is past `PYTHON_AGENT_AUDIT_RETENTION_DAYS`
- **THEN** the next audit-retention iteration deletes it and increments `agent_audit_retention_deleted_total`

#### Scenario: Below-floor audit retention refuses startup

- **WHEN** `PYTHON_AGENT_AUDIT_RETENTION_DAYS=100` is set
- **THEN** startup raises `ValueError: PYTHON_AGENT_AUDIT_RETENTION_DAYS must be >= 365; got 100` and the process exits

### Requirement: Kill switch and disabled-state behavior

Both endpoints SHALL be gated by `PYTHON_AGENT_GDPR_ENABLED` (default true). When false, both SHALL return 503 with the standard error envelope (`{"error": ..., "details": ...}` with a static safe detail); no data SHALL be read or written, and no audit row SHALL be appended (the forbidden operation never happened).

#### Scenario: Disabled responds 503 without touching data

- **WHEN** `PYTHON_AGENT_GDPR_ENABLED=false` and a user calls export or erase
- **THEN** the response is 503 with the standard envelope, row counts are unchanged, and no audit row is appended

#### Scenario: Re-enable restores the endpoints

- **WHEN** the flag flips back to true
- **THEN** both endpoints behave per their requirements with no restart-dependent state left behind
