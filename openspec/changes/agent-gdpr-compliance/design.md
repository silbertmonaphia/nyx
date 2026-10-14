# Design

## Context

Slices 1–3 left four stores carrying user data: `agent.chat_sessions`, `agent.chat_messages`, `agent.chat_user_prefs`, `agent.runs`, plus langGraph's `langgraph.checkpoint_*` (keyed by thread id = run id). This slice adds the export surface, the erase surface, and an append-only audit log. See [`proposal.md`](./proposal.md) for motivation; the `agent-gdpr-erasure` spec carries the behavioral contract.

Anchors: `agent/api/` package exists from slice 2 (`chat_sessions.py` + header enforcement pattern); `RetentionCron._chat_iteration` is the template for a new audit iteration; `RunMetadata` owns `agent.runs` SQL; the checkpointer (`adelete_thread`) is already injected into `RetentionCron` in `main.py`.

## Goals / Non-Goals

**Goals:**
- One-call export containing every row tied to the subject, plus the checkpoint-thread id list
- Erase that walks all five stores in a defined order and is idempotent
- Audit log that proves the operation happened without becoming a shadow copy
- Operator playbook executable from the docs alone

**Non-Goals:**
- A user-facing SPA privacy UI (the endpoints are private-network; a UI proxy is a future change)
- Checkpoint content export (langGraph deserialization is not a stable surface; thread ids + `adelete_thread` make the id the complete unit)
- Async/background erasure jobs (erase is synchronous; the data volume per user is bounded by retention and typically small)
- Anonymization/pseudonymization mode (erasure is full deletion; partial anonymization is out of scope)

## Decisions

### D1. Export is a single JSON body, not a streamed archive

User data here is small (chat history capped by retention; prefs are dozens of rows). A single JSON body with array fields is trivially inspectable, diffable, and testable; a streaming/ZIP format adds machinery for no requirement. If a future user has 10⁵ messages, chunking becomes worth a follow-up — the endpoint shape (`exported_at` + arrays) stays compatible with pagination addition.

### D2. Checkpoint content is represented by thread ids, not deserialized

langGraph's checkpoint format is internal (msgpack rows across `checkpoint_blobs`/`checkpoint_writes`). Deserializing it for export would couple us to its schema across langgraph versions. The erasure unit is the *thread* (`adelete_thread`), so exporting the thread id list is a complete representation of "what holds your state and can be deleted". The export includes the pre-restore caveat in the docs (backups).

### D3. Erase order is messages → sessions → prefs → runs → threads → audit

Reverse-dependency order: children before parents (messages before sessions is technically redundant given FK CASCADE, but explicit statements give exact counts for the response/audit and don't rely on cascade side effects). Threads last because `adelete_thread` is the only non-SQL step and its failures must be contained; the audit row is written even when some thread deletions fail (with counts reflecting reality) — the erase *request* was served.

### D4. Audit rows carry counts, never content

The audit log's purpose is proving the operation occurred (who, when, what kind, how many rows). If `payload_summary` carried content, the erase endpoint would create a shadow copy of the data it just deleted — a well-known compliance anti-pattern. Enforced by the spec ("counts only") and by the store API shape (`record(user_id, action, summary: dict[str, int])`).

### D5. Audit retention is its own iteration with a floor

`PYTHON_AGENT_AUDIT_RETENTION_DAYS` default 1095 (3 years), validated ≥ 365 at startup. The 3-year default outlives the 1-year data retention deliberately: erase/export evidence remains verifiable after the underlying data would have been reaped anyway. The iteration runs in `RetentionCron` alongside run/chat iterations (same cadence, same failure isolation) rather than a second cron — one operational knob.

### D6. Kill switch is `PYTHON_AGENT_GDPR_ENABLED` (default true)

Erasure is legally load-bearing; shipping it default-on is the safe posture (a disabled-by-default compliance endpoint is a latent incident). The kill switch exists for the rollout drill (§3.4) and for an operator who must temporarily freeze destructive endpoints — when false, both endpoints 503 without reading data and without writing audit rows ("the forbidden operation never happened").

### D7. Erase helpers live beside their tables' stores

`delete_all_for_user(user_id)` on `ChatStore` (sessions+messages+prefs share the pool/lifecycle) and `RunMetadata.delete_all_for_user(user_id) -> list[str]` (returns run ids so the endpoint can drive `adelete_thread`). Keeping SQL in the store modules preserves the "stores own SQL" discipline; the endpoint composes them and never writes SQL itself. Every statement filters by `user_id` (pinned by the cross-user test) — erasure must be the *least* likely code path to have a missing owner predicate.

## Risks / Trade-offs

- **[Backups resurrect erased data]** A restored pre-erasure backup brings the user back. → Mitigation: the documented playbook (rollout §3.2) defines "restore for disaster recovery only, then re-apply recorded erasures from the audit log" — the audit table is precisely what makes re-application enumerable.
- **[A partial erase leaves inconsistent counts]** If the process dies mid-erase, some tables are cleaned and others aren't; the idempotent contract makes a re-run safe. → Mitigation: idempotency test; the response counts tell the operator exactly what each stage deleted.
- **[`adelete_thread` failure leaves checkpoint content]** The metadata row is gone but the checkpoint thread lingers (still keyed by an untraceable uuid). → Mitigation: keep the run ids from the failing stage in the warn log; the `threads` count in the response and audit makes the shortfall explicit. The rollout smoke (§1.2) verifies the count math live.
- **[Export endpoint as a data-exfiltration vector]** An attacker with a valid user JWT can export that user's own data — that's the intended subject-access behavior. → Mitigation: strictly owner-scoped (header = JWT-verified id; no parameter can point at another user); route sits behind the same network boundary as the rest of the agent service; audit rows record every export.
- **[Large export blocks the event loop]** The queries are small; JSON serialization of a bounded result set is fine. → Mitigation: measure in the rollout if a heavy user appears; the single-body design (D1) is the seam for later pagination.

## Migration Plan

Alembic `0006_data_audit_log` (down_revision `0005_chat_user_prefs`). New table only; no backfill. Rollback: `alembic downgrade -1` drops the audit table; `PYTHON_AGENT_GDPR_ENABLED=false` is the operational rollback without code change (both endpoints 503; audit retention keeps running independently).
