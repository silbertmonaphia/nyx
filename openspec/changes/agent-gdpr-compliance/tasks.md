# Tasks

## 1. Schema + settings

- [ ] 1.1 Add alembic `0006_data_audit_log.py` (down_revision `0005_chat_user_prefs`): `agent.data_audit_log(id bigserial PK, user_id bigint NOT NULL, action text NOT NULL, payload_summary jsonb NOT NULL DEFAULT '{}', requested_at timestamptz NOT NULL DEFAULT now())`; index `(user_id, requested_at DESC)` and `(requested_at)` for the retention sweep. Verify: `alembic upgrade head` then `downgrade -1` then `upgrade head` against the local Docker Postgres.
- [ ] 1.2 Add `gdpr_enabled: bool = True` and `audit_retention_days: int = 1095` to `config.py` + `_validate_gdpr` (raise when `audit_retention_days < 365`) per the spec's startup scenarios. Verify: defaults load; `PYTHON_AGENT_AUDIT_RETENTION_DAYS=100` raises; `PYTHON_AGENT_GDPR_ENABLED=false` loads.

## 2. Audit store

- [ ] 2.1 New `backend/agent_py/agent/audit.py`: `AuditLog` (same pool/`_with_retry` posture) with `record(user_id, action, summary: dict[str, int])` (INSERT only), `retention_delete(days) -> int` (DELETE ... WHERE requested_at < now() - days, RETURNING count). Module counter `agent_audit_retention_deleted_total`. `payload_summary` carries counts only — the method signature enforces it (`dict[str, int]`). Verify: `python -m pytest tests/test_gdpr.py -v` covers record + readback + retention delete + counter.

## 3. Erase helpers in the stores

- [ ] 3.1 `ChatStore.delete_all_for_user(user_id) -> dict[str, int]` — one transaction: delete the user's `chat_messages` (join via their sessions, INCLUDING soft-deleted sessions), delete `chat_sessions`, delete `chat_user_prefs`; return `{messages, sessions, prefs}` counts. Every statement carries the `user_id` predicate. Verify: `python -m pytest tests/test_gdpr.py -v` — soft-deleted sessions removed too; a foreign user's rows untouched (counts compared before/after).
- [ ] 3.2 `RunMetadata.delete_all_for_user(user_id) -> list[str]` — DELETE the user's `agent.runs` rows RETURNING id (captures ids for checkpoint GC). Verify: covered by `tests/test_gdpr.py` — ids returned, foreign runs untouched.

## 4. Endpoints

- [ ] 4.1 New `backend/agent_py/agent/api/gdpr.py`: `GET /agent/me/export` and `DELETE /agent/me` per the `agent-gdpr-erasure` spec. Export composes reads from ChatStore/RunMetadata/audit; erase composes the stores in the D3 order, calls `checkpointer.adelete_thread(run_id)` per captured run id via the app-state checkpointer, counts thread successes/failures, then appends the audit row. Both enforce `X-Nyx-User-Id` (400 on missing/non-integer), 503 with the standard envelope when `gdpr_enabled=false` (no reads, no audit write), and emit the `agent.gdpr.export` / `agent.gdpr.erase` spans. Idempotency: second erase → zero counts, 200. Register the router in `main.py` (the slice-2 `api` package). Verify: `python -m pytest tests/test_gdpr.py -v` — export completeness + cross-user exclusion; erase counts + all tables zeroed + soft-deleted removed; double erase safe; `adelete_thread` called once per run id (stub saver records calls); one thread failure → contained, counts reflect it, still 200 + audit row; disabled → 503 + no audit row.

## 5. Audit retention iteration

- [ ] 5.1 Extend `RetentionCron` with an audit iteration (span `agent.audit.retention`, counter `agent_audit_retention_deleted_total`), running alongside the run/chat iterations with the same failure isolation. Wire the `AuditLog` into the cron construction in `main.py`. Audit retention runs independently of `gdpr_enabled`. Verify: `python -m pytest tests/test_gdpr.py -v` — backdated audit row reaped, counter ticks; `gdpr_enabled=false` does not skip it.

## 6. Docs

- [ ] 6.1 Extend `docs/agent-deployment.md`: data inventory table (table → what it holds → retention window → erasure coverage), export/erase operator playbook (exact curl invocations, response shapes, the restore-then-reapply-erase rule), retention matrix (chat 365d / audit 1095d / run 30d), and the kill-switch. Add the cross-link in `SECURITY.md` (data-handling section). Update `backend/agent_py/.env.example` + `README.md` env tables. Verify: `git diff --check`; every table in the inventory exists in the migrations (grep each `agent.*` table name); README env count matches code.
- [ ] 6.2 Update `FUTURE_AGENT.md` §2.1 (tick the GDPR/privacy-review bullet; reference this change) and `AgentMemoryRoadmap.md` Slice 4 (tick after archive). Verify: roadmap diff shows all four slices ticked.

## 7. Self-review

- [ ] 7.1 Spawn the `test-reviewer` agent against the Slice 4 diff; fix every CRITICAL/HIGH before archive; file `review.md`. Pay special attention to the cross-user isolation predicates in §3.1/§3.2 (the highest-blast-radius diff lines in the plan).

## 8. E2E

- [ ] 8.1 Live stack (side agent instance): run the rollout sibling's §1.1–§1.4 smokes and record the transcript as `e2e-evidence.md` here — export completeness, erase walk (all tables + `langgraph.checkpoint_*` thread gone), double-erase idempotency, cross-user isolation, audit retention reap. If the live stack is unavailable, the §2–§5 unit coverage is the accepted substitute with the rationale recorded. The Playwright leg applies only if a UI surface ships; with operator-only endpoints, record that the curl smokes + unit coverage are the accepted substitute per the rollout sibling's §2.1.

## 9. Archive

- [ ] 9.1 Run `openspec archive agent-gdpr-compliance --yes`; verify `openspec validate --archived` passes; change moves to `openspec/changes/archive/2026-10-14-agent-gdpr-compliance/`.
