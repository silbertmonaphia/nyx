# E2E evidence — agent-chat-history-ui (2026-10-14)

Environment: side stack on spare ports (`scripts/e2e-side-stack.sh up` — Go :8180, Python agent :9100, Vite :5273 with `VITE_API_URL=http://localhost:8180/api`), against the shared infra (Postgres :5433 `nyx`, Redis :6380) and the host vLLM (:8000). The operator's own stack (:8080/:9000/:5173) was left untouched.

**Playwright (serial, CI mode — `--workers=1`): `26 passed (1.7m)` — the full suite.** Includes:
- `chat-history-sidebar.spec.ts` — 6/6: empty rail; reload restores + replays user AND assistant rows; `+ New chat` continues in a new session; click-to-replay + active-row no-op; delete clears + drops the row; 600px drawer open/close-on-tap.
- `chat-rag.spec.ts` — 7/7, including the rewritten `closing and reopening the dialog restores the active conversation` (the old "closing resets the history" contract is superseded by this change — see the test's comment) and the strengthened assistant-replay assertion.
- `chat-agent-planner.spec.ts`, `chat-agent-retry.spec.ts`, `chat-web-tools.spec.ts` (recorded-wire replays + live), `chat-langfuse-tracing.spec.ts`, `chat-empty-bubble.spec.ts`, `feed-crud.spec.ts`.

REST-level smokes during development (curl through the side Go):
- `POST /api/register` → token; `POST /api/chat/sessions` → `{session_id, started_at}`; `GET /api/chat/sessions?limit=10` → the row with `message_count`; CORS preflight from the :5273 origin echoes correctly.

Parallel-local note: the same suite at default worker count flakes on live-LLM tests (shared single-GPU vLLM queueing under ~26 concurrent browser contexts → the repo's documented slowdown-burst timeout sensitivity; CI runs serial — `playwright.config.ts` `workers: CI ? 1 : undefined`). Serial is the gate and is green.

## Defects this gate caught (both fixed in the change)

1. **Session continuation was unimplemented** — an echoed session id was dropped by the run-resume decision, so every turn minted a new session (the rail grew one row per message; `+ New chat` sessions could never receive their first message). Fixed in `agent/main.py` (live-session branch → continuation) + `tests/test_session_continuation.py`; the idle-boundary design was removed in the same round (review M1).
2. **Terminal persistence lost on client disconnect** — the SPA closes the stream at `terminator`; Go cancels upstream; the agent generator was cancelled mid-`finally`, losing BOTH the assistant row and the terminal run transition for fully-streamed answers (observed in the DB: user row only, `agent.runs` stuck `running`). Fixed with the detached, shielded finalize task in `agent/main.py`; pinned by `test_disconnect_during_finalize_still_persists` and the assistant-replay assertions in two e2e specs.
