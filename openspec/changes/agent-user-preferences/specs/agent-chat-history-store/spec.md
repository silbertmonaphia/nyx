# Spec Delta

## Purpose

(see existing capability)

## ADDED Requirements

### Requirement: Derived preferences follow session retention

When a session is hard-deleted by the chat-retention cron, the service SHALL delete every `agent.chat_user_prefs` row whose `source_session_id` is that session, in the same retention iteration. Soft-deletion alone (phase 1) SHALL NOT touch prefs — a soft-deleted session's preferences remain injectable until the session is physically removed, matching the "grace window is a buffer" semantics of the storage layer.

#### Scenario: Hard-delete collects derived prefs

- **WHEN** the chat retention cron hard-deletes session S and a prefs row has `source_session_id = S`
- **THEN** the prefs row is deleted in the same iteration
- **AND** `agent_chat_retention_deleted_total` is unaffected (prefs are not counted as sessions)

#### Scenario: Soft-delete leaves prefs injectable

- **WHEN** session S is soft-deleted (phase 1) but not yet hard-deleted
- **THEN** prefs rows with `source_session_id = S` still exist and remain eligible for planner injection
