# Tasks

Rollout-split: no rollout gates are foreseen — no feature flag or env flip, and the nullable migration is verified in dev/CI. No `agent-chat-step-replay-rollout` sibling is created.

## 1. Coordination

- [x] 1.1 Agree land order with the `agent-per-turn-run-ids` owner before editing the shared files (`agent/main.py`, `agent/chat_store.py`, `agent/api/chat_sessions.py`); record the agreement and the files each change owns. Verify with the peer's acknowledgment (message id). Evidence: nyx-f8 confirmed it owns no run-ids work and has not touched the three files (message `0eedd783`), nyx-f0 and nyx-6c confirmed no overlap (`e286d50a`, `37723ba9`); treated as unowned and safe to proceed.

## 2. Storage layer

- [x] 2.1 Add `backend/agent_py/migrations/versions/0008_chat_message_steps.py` (`down_revision = "0007_chat_message_usage_split"`) adding a nullable `steps` JSONB column to `agent.chat_messages`. Verify `alembic upgrade head` on a scratch DB, `\d agent.chat_messages` shows the column, and a downgrade/upgrade round-trip succeeds. Evidence: upgrade → column present, downgrade to 0007 → absent, upgrade → present, against `nyx_test`.
- [x] 2.2 Thread `steps` through `chat_store.py`: `MessageRow.steps`, `record_message(..., steps=...)` JSONB insert, `list_messages` and `list_all_messages_for_user` selects, `_row_to_message` decode. Extend `tests/test_chat_store.py` with a populated-trace round-trip and a NULL-trace round-trip; verify `pytest tests/test_chat_store.py` passes. Evidence: populated 3-step trace and NULL round-trips green.

## 3. Step-trace accumulation

- [x] 3.1 Implement the `StepTrace` fold (new `agent/step_trace.py`) for `plan`, `step_start`, `tool_call`, `tool_result`, `confirm_request`, and `step_end`: one entry per step id, plan order, latest value wins, empty trace → `None`, unknown frames ignored. Add unit tests covering every frame kind, duplicate/re-emitted frames, and the empty case; verify `pytest tests/test_step_trace.py` passes. Evidence: 13 tests green, including the tool-frame-only `kind: "tool"` default, stale-reason clearing on a late ok `step_end`, and a payload non-mutation pin. Failure reasons are static classifications (safe helper text or exception class name), never provider `repr`.
- [x] 3.2 Wire the fold into `main.py`'s drain (`assistant_steps = StepTrace()`, feed every forwarded event) and into the finalize `record_message` call; a trace-serialization failure degrades to `steps=None` with a WARN, never suppressing the assistant row. Add an integration test asserting the persisted assistant row carries the planned steps and that a no-plan run persists NULL; verify the relevant `pytest` module passes. Evidence: `test_session_continuation.py` asserts a planned run persists `[{"id":"step_1","kind":"llm","status":"ok"}]`, a no-plan run persists NULL, a non-JSON-serializable trace lands the assistant row with `steps IS NULL`, and a PostgreSQL-rejected trace (NUL escape) retries without the trace while the assistant row and `ended_at` still land.

## 4. API surfaces

- [x] 4.1 Add `"steps": message.steps` to `agent/api/chat_sessions.py::_message_json` and `agent/api/gdpr.py::_message_json`. Extend `tests/test_chat_sessions_api.py` and `tests/test_gdpr_endpoints.py` with populated and NULL assertions; verify both modules pass. Evidence: both modules green; wire fixture `step_trace.json` pins the exact serializer output and is consumed by the frontend parser test.

## 5. Frontend replay

- [x] 5.1 Add `SessionStep` and `SessionMessage.steps` in `frontend/src/features/chat/types/chat.ts`, and parse the array defensively in `chatService.toSessionMessage` (drop malformed entries, NULL stays NULL). Extend `chatService.test.ts`; verify `npm test -- chatService`. Evidence: 45 tests green, including the Python-produced fixture round-trip and an explicit `steps: []` → `null` case.
- [x] 5.2 Make `toReplayMessage` prefer a non-empty `steps` trace and map it to `ChatStep[]`, keeping the existing `tool_calls` fallback when `steps` is NULL or empty. Extend `chatStore.test.ts` with native `rag`/`llm` replay, legacy row fallback, and malformed-entry tolerance; verify `npm test -- chatStore`. Evidence: store tests green; full chat feature suite 199 tests green.

## 6. End-to-end and gates

- [x] 6.1 Extend `frontend/tests/e2e/chat-token-usage.spec.ts` (or add `frontend/tests/e2e/chat-step-replay.spec.ts`) to assert the live step rows (`chat-agent-steps` / `chat-agent-step`) and the three-part usage line survive closing and reopening the panel, and switch away/back through the session rail. Run against the side stack and record the result. Evidence: new `chat-step-replay.spec.ts` (nyx-f0's assertions untouched) → 2/2 passed against `PLAYWRIGHT_BASE_URL=http://localhost:5273`, `--workers=1`. The first run failed the rail-switch case on a transient empty `POST /api/chat/sessions` response (side-stack cold start); the immediate rerun was green, and a post-review-fix rerun was 2/2 green again.
- [x] 6.2 Run the full gates and record results: agent `pytest` both with and without `TEST_ASYNC_PG_DSN`, `cd frontend && npm test`, `npm run lint`, `npm run build`. The Go tree is untouched; no backend Go run is required. Evidence: with DB 577 passed / 1 skipped (one rate-limit burst test failed once and passed in isolation and on the full rerun — pre-existing timing flake, unrelated files), bare 427 passed / 151 skipped, frontend 408 passed, ESLint clean, Vite build green, `scripts/check_no_comments.py --range HEAD` green and the new untracked files manually scanned (no comment lines).
- [x] 6.3 Update every doc surface that enumerates the replay/export message fields (grep `prompt_tokens` and `tool_calls` under `docs_agent/` and `docs/agent-deployment.md`) to include `steps`. `README_FULL.md` is currently claimed by the `toolchain-reproducibility` workstream — coordinate before editing it or hand that edit over. Verify each documented field list matches the shipped wire shape. Evidence: `docs_agent/AgentMemoryRoadmap.md` schema line and `docs/agent-deployment.md` data inventory + export example updated. `README_FULL.md` contains no message-field enumeration, so no edit or hand-off was needed.
