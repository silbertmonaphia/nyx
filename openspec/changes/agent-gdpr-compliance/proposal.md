# Proposal

## Why

The roadmap locked "Retention floor ≥ 1 year" and flagged GDPR as non-negotiable at that floor. Slice 1 started storing personal data (`chat_sessions`, `chat_messages`), Slice 3 adds derived memory (`chat_user_prefs`), and run state lives in `agent.runs` + `langgraph.checkpoint_*` — but there is no export path and no erasure path anywhere: a data-subject request today would be served by hand-written SQL with no audit trail. This slice seals the exits: an owner-scoped export endpoint, an owner-scoped erase endpoint that walks every table in a defined order (including langGraph checkpoint threads), an append-only audit log (3-year retention, separate from the 1-year chat retention), and the data-inventory documentation that makes the operator playbook executable. It is the last slice of `AgentMemoryRoadmap.md` and the precondition for any future production deployment of the memory capability.

## What Changes

- New endpoints (Python agent, owner-scoped via `X-Nyx-User-Id`, no admin override; gated by `PYTHON_AGENT_GDPR_ENABLED`, default true):
  - `GET /agent/me/export` → JSON dump: every `agent.chat_sessions` row, every `agent.chat_messages` row (content included — it is the subject's own data), every `agent.chat_user_prefs` row, every `agent.runs` row, and the langGraph checkpoint thread ids for the user's runs (ids only — checkpoint content requires langGraph deserialization and is covered by the thread-id list because the thread id IS the unit of erasure). Response streams in one JSON body; 404-free (an empty user exports empty sets).
  - `DELETE /agent/me` → physical delete in this exact order: `chat_messages` (by session join) → `chat_sessions` → `chat_user_prefs` → `agent.runs` (+ `adelete_thread` for every run's checkpoint thread) → append the audit row. Returns `{deleted: {sessions, messages, prefs, runs, threads}}`. Idempotent: a second call returns 200 with zero counts.
- New table `agent.data_audit_log(id bigserial, user_id bigint, action text, payload_summary jsonb, requested_at timestamptz)` — append-only (no UPDATE/DELETE from application code except the retention cron), retention `PYTHON_AGENT_AUDIT_RETENTION_DAYS` (default 1095 = 3 years). Actions: `export`, `erase`. `payload_summary` carries counts (never content — the audit log must not become a shadow copy of the data it audits).
- New retention iteration for the audit log in `RetentionCron` (span `agent.audit.retention`, counter `agent_audit_retention_deleted_total`).
- `backend/agent_py/agent/audit.py` (new): `AuditLog` store (same pool/retry posture) with `record(user_id, action, summary)` + `retention_delete(days) -> int`.
- `backend/agent_py/agent/api/gdpr.py` (new): the two endpoints on the slice-2 `api` package, same header enforcement + OTel span pattern.
- Docs: `docs/agent-deployment.md` gains the data inventory table (which table holds what, which retention applies), the export/erase operator playbook (including the "restore-then-reapply-erase" note), and the SLA statement; `SECURITY.md` gains a cross-link. `FUTURE_AGENT.md` §2.1 marks the slice complete.
- **Wire**: two new JSON REST endpoints on the private agent network (reachable by the Go backend / operators inside the compose network; a user-facing SPA proxy is explicitly deferred until a privacy UI exists). No SSE changes.

## Capabilities

### New Capabilities

- `agent-gdpr-erasure`: the export surface (complete, owner-scoped, streaming one JSON body), the erase surface (defined delete order, checkpoint-thread GC, idempotent), the audit log's shape/actions/retention (3 years, append-only, counts-not-content), and the disabled-state behavior (`PYTHON_AGENT_GDPR_ENABLED=false` → 503 on both endpoints).

### Modified Capabilities

- `agent-python-service`: env-var list gains `PYTHON_AGENT_GDPR_ENABLED` (default true) and `PYTHON_AGENT_AUDIT_RETENTION_DAYS` (default 1095, must be ≥ 365 so the audit trail always outlives the data it audits) + validators.
- `agent-observability`: counter list gains `agent_audit_retention_deleted_total`; span list gains `agent.audit.retention` and the two endpoint spans (`agent.gdpr.export`, `agent.gdpr.erase`).
- `agent-chat-history-store`: the erasure path is a second, owner-scoped deleter alongside retention — the storage contract gains the sentence that owner erasure removes soft-deleted rows too (retention is not a precondition for erasure).

## Impact

- **Code**: `backend/agent_py/agent/audit.py` (new), `backend/agent_py/agent/api/gdpr.py` (new), `backend/agent_py/agent/api/__init__.py` (register), `backend/agent_py/agent/config.py` (2 settings + validators), `backend/agent_py/agent/main.py` (router registration; audit store construction), `backend/agent_py/agent/retention.py` (audit iteration), `backend/agent_py/agent/chat_store.py` (owner-erase helpers: delete-all-for-user across sessions/messages/prefs), `backend/agent_py/agent/run_metadata.py` (owner-erase helper for `agent.runs` + return the run ids for checkpoint GC), `backend/agent_py/migrations/versions/0006_data_audit_log.py` (new).
- **Wire**: two JSON endpoints on the agent side (no SPA surface in this slice).
- **Dependencies**: none new.
- **Docs**: `docs/agent-deployment.md` (data inventory + playbook + retention matrix), `SECURITY.md` (cross-link), `backend/agent_py/.env.example`, `FUTURE_AGENT.md` §2.1, `AgentMemoryRoadmap.md` Slice 4 tick.
- **Tests**: `backend/agent_py/tests/test_gdpr.py` (export completeness incl. a second user's rows present-but-excluded; erase order + counts; idempotent double-erase; checkpoint-thread GC called per run id; audit rows for both actions with counts-not-content; disabled → 503; audit retention reaping), plus a retention-coupling test in `test_retention_chat.py` if the audit iteration lands there.
- **Rollout gates tracked in** `openspec/changes/agent-gdpr-compliance-rollout/` (per the rollout-split convention).
