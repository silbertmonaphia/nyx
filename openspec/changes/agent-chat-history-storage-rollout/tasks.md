# Tasks

> Rollout gates split out of `agent-chat-history-storage` per the rollout-split convention (`openspec/config.yaml`). The implementation change is code-complete and review-approved (archived as `openspec/changes/archive/2026-10-14-agent-chat-history-storage/`); these gates need a deployed stack, real clocks (retention windows), and observation windows. PM-owned: do not archive from a dev session. Behaviour spec: `openspec/specs/agent-chat-history-store/spec.md`. Deploy-doc reference: `docs/agent-deployment.md`.

## 1. Production deploy gates

- [ ] 1.1 Migration on the deploy host — the change ships alembic `0004_chat_sessions_and_messages`. On the deploy path (compose `agent-migrate` one-shot OR inline `alembic upgrade head` on boot), verify: `alembic current` reports `0004` on the deployed DB; `agent.chat_sessions` + `agent.chat_messages` exist with both indexes (`chat_sessions_user_active_idx` partial index, `chat_messages_session_created_idx`); a chat through the deployed SPA leaves 2 rows (user + assistant) for the test user. Record the transcript.
- [ ] 1.2 Retention observation window — after the deploy, watch `agent_chat_retention_deleted_total` on the deployed `:9001/metrics` for ≥ 1 week. Expected: 0 increments (no session is 365 days old yet on a fresh deploy) and the counter is registered/visible. Separately, seed a run with a backdated `last_active_at` in staging and verify the two-phase behavior live: first retention tick soft-deletes, hard-delete only after the grace window. Record both.
- [ ] 1.3 Kill-switch drill in staging — set `PYTHON_AGENT_CHAT_RETENTION_ENABLED=false`, restart the agent, verify no chat rows are deleted (backdated rows stay) while run-retention continues; flip back to `true` and verify the next tick resumes chat retention. Record. No migration rollback needed — the tables stay.
- [ ] 1.4 Storage growth check — after ≥ 1 week of production traffic, record row counts (`chat_sessions`, `chat_messages`), table+index sizes (`pg_total_relation_size`), and the write-rate estimate. Confirm the numbers are within the capacity budget before closing this gate.
- [ ] 1.5 Deploy notes — update `docs/agent-deployment.md`: the three new env vars + defaults, the one-year floor (startup refuses < 365), the two-phase retention semantics, the kill-switch rollback path, and the manual check query (`SELECT count(*) FROM agent.chat_sessions WHERE deleted_at IS NOT NULL`). Verify: documented defaults match the shipped config.

## 2. Archive

- [ ] 2.1 After §1 is checked: archive this change (PM/operator only — the implementer leaves this unchecked). Verify `openspec validate --archived` passes post-archive.

## References

- `openspec/changes/archive/2026-10-14-agent-chat-history-storage/` — implementation change (proposal, design, specs, tasks; archived 2026-10-14).
- `openspec/specs/agent-chat-history-store/spec.md` — canonical behaviour spec synced at archive.
- `docs/agent-deployment.md` — rollout order, env vars, verification queries.
- `openspec/changes/agent-stale-run-sweeper-rollout/tasks.md` — sibling model this change mirrors.
- `AgentMemoryRoadmap.md` — Slice 1 of the 4-slice plan; this rollout is its production tail.
