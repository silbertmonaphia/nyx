# Tasks

> Rollout gates split out of `agent-gdpr-compliance` per the rollout-split convention (`openspec/config.yaml`). The implementation change is code-complete and review-approved by the time this change is reached; these gates need a deployed stack, backup infrastructure, and policy sign-off. PM-owned: do not archive from a dev session. Behaviour spec: `openspec/specs/agent-gdpr-erasure/spec.md` (once synced). Deploy-doc reference: `docs/agent-deployment.md`.

## 1. Dev-stack live smokes (local stack)

- [ ] 1.1 Export smoke — full stack up (side agent instance if the operator's is running). Register a user, run 2 chats with a preference-bearing message. `curl -H 'X-Nyx-User-Id: <u>' localhost:<side>/agent/me/export` → JSON contains every session, every message, the prefs row, and the run's langGraph thread ids; no other user's data appears. Record the JSON shape (redact content).
- [ ] 1.2 Erase smoke — `curl -X DELETE -H 'X-Nyx-User-Id: <u>' localhost:<side>/agent/me` → 200; re-run the export → empty; `SELECT count(*)` on `agent.chat_sessions` / `agent.chat_messages` / `agent.chat_user_prefs` / `agent.runs` for that user = 0; the langGraph checkpoint thread is gone (`SELECT count(*) FROM langgraph.checkpoint_* WHERE thread_id = '<run_id>'` = 0); `agent.data_audit_log` has the erase row. Double-delete → 200 (idempotent), audit gains a second row. Record.
- [ ] 1.3 Cross-user isolation — user B's export/erase attempt with user A's data present: export returns only B's rows; deleting B leaves A untouched. Record.
- [ ] 1.4 Audit retention smoke — backdate an audit row past `PYTHON_AGENT_AUDIT_RETENTION_DAYS` (SQL), run one retention iteration (short-interval env override), verify the row is reaped and the counter ticks. Record.

## 2. Browser e2e

- [ ] 2.1 Playwright run (if the change ships any UI-visible surface; if the endpoints remain operator-only per design, record that no browser leg applies and the curl smokes + unit coverage are the accepted substitute). If a spec exists: `cd frontend && npm run test:e2e -- tests/e2e/chat-gdpr.spec.ts`. Record.

## 3. Production gates

- [ ] 3.1 1-week CI green — agent pytest suites (including the new gdpr tests) green in CI for ≥ 1 week before any production flip. Wait-and-record.
- [ ] 3.2 Staging erasure drill — full erasure request playbook on staging: export → verify → erase → verify empty → audit verified → confirm restore expectations documented (an erased user must NOT come back when a pre-erasure backup is restored — the documented answer is "backups are restored only for disaster recovery; the audit log records the erasure so the erase is re-applied post-restore", and this drill proves the playbook wording). Record the whole drill.
- [ ] 3.3 Backup / retention policy — document and configure: DB backup schedule + retention, `PYTHON_AGENT_AUDIT_RETENTION_DAYS` (default 1095 = 3 years) vs chat retention (365 days) rationale, and the restore-then-reapply-erase runbook. Sign-off recorded in `docs/agent-deployment.md`.
- [ ] 3.4 Rollback drill — flip `PYTHON_AGENT_GDPR_ENABLED=false` in staging: export/erase return 503 with the standard envelope; no data is touched; flip back and the routes work again. Record.
- [ ] 3.5 Production flip — after 3.1–3.4, enable in production; record the flip + the operator playbook paths in `docs/agent-deployment.md`.

## 4. Archive

- [ ] 4.1 After §1–§3 are checked: archive this change (PM/operator only — the implementer leaves this unchecked). Verify `openspec validate --archived` passes post-archive.

## References

- `openspec/changes/agent-gdpr-compliance/` — implementation change (proposal, design, specs, tasks).
- `docs_agent/AgentMemoryRoadmap.md` — Slice 4 of the 4-slice plan.
- `SECURITY.md` — the data-inventory cross-link the implementation adds.
- `openspec/changes/agent-chat-history-storage-rollout/tasks.md` — sibling model.
