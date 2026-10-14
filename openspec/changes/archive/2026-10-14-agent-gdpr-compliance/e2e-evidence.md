# E2E evidence — agent-gdpr-compliance (2026-10-14)

Environment: side stack (`scripts/e2e-side-stack.sh`: agent :9100 with the slice-4 code + migration 0006 applied on boot, Go :8180, Vite :5273) + shared infra (Postgres :5433). Operator stack untouched. The GDPR endpoints are agent-side (private network, `X-Nyx-User-Id` header — no Go/SPA path in this slice by design), so the smokes run directly against :9100; the browser suite runs as the cross-slice regression gate.

## Live operator smokes (user 7777, real stack)

1. **Seed** — one single-turn run (session S1 = its run id) + one continuation (session S2 created via `POST /agent/sessions`, then a chat echoing `run_id: S2` — no `agent.runs` row by design). Pre-erase checkpoint material verified: `langgraph.checkpoint_blobs` = 18 rows for S1 **and 18 for S2** (the continuation thread the compliance fix targets).
2. **Export** (`GET /agent/me/export`) → `sessions: 2, messages: 4 (all carrying session_id linkage), prefs: 0, runs: 1, checkpoint_thread_ids: [S1, S2]` — the continuation thread is listed despite having no runs row. Audit row appended: `export {sessions:2, messages:4, prefs:0, runs:1, threads:2}` (counts only).
3. **Erase** (`DELETE /agent/me`) → `{"deleted":{"sessions":2,"messages":4,"prefs":0,"runs":1,"threads":2}}` — exact counts. Post-state: `checkpoint_blobs` = **0 for BOTH threads** (including S2), `chat_sessions` for the user = 0, re-export = all zeros. Audit row: `erase` with the same counts.
4. **Idempotency** — second erase → all-zero counts, 200, second zero-count `erase` audit row.
5. **Audit retention** — two backdated `export` rows (400 days) → one `_audit_iteration` tick reaped exactly those two (`agent_audit_retention_deleted_total` 0→2), the two fresh `erase` rows untouched.

## Regression gate

**Full Playwright suite, serial (CI mode): 27 passed (2.6m)** against the slice-4 stack — every chat/feeds spec unchanged, including the slice-2 sidebar suite and the slice-3 prefs spec.

## Unit/integration coverage (backend suite: 437 passed, 1 skipped on nyx_test)

`tests/test_gdpr_endpoints.py` (14) — every spec scenario at the HTTP surface (export completeness with a second user present-and-excluded; empty export; soft-deleted inclusion; erase walks all stores with per-thread GC recorded; contained thread failure; counts-not-content audit; 503 kill switch touching nothing; 400 headers; audit-retention reap under `gdpr_enabled=false`; span attributes). `tests/test_gdpr_stores.py` (12) — store-level erasure/read/audit semantics. `tests/test_retention_chat.py` — session hard-delete GCs its checkpoint thread (recorder), prefs coupling, orphan sweep.
