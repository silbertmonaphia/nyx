# Tasks

## 1. Run store: session-linked per-turn rows

- [ ] 1.1 Write the failing unit tests first: `create_run(user_id, session_id="<uuid>")` inserts `agent.runs` with `session_id` set and the full running-state shape; `session_id=None` leaves it NULL; lifecycle (`touch_heartbeat`, `mark_completed`, `mark_failed`) behaves identically on session-linked rows. Run: `cd backend/agent_py && .venv/bin/python -m pytest tests/test_run_metadata.py -q` (extend the existing file).
- [ ] 1.2 Alembic revision adding nullable `agent.runs.session_id` (uuid, no FK — session rows are soft-deleted, runs are GC'd independently); `make -C backend agent-alembic` equivalent per `backend/agent_py` README applies on a dev DB and `alembic downgrade -1` reverses cleanly. Verify: migration up/down on the dev database.
- [ ] 1.3 Implement `create_run(user_id, session_id=None)` and the `_RUN_COLUMNS` addition; verify with 1.1's suite plus the full agent unit run `pytest tests/ -q` (bare config, no `TEST_ASYNC_PG_DSN`).

## 2. Entrypoint: per-turn minting on continuation

- [ ] 2.1 Write the failing adversarial tests for three-state detection against the entrypoint handler: echoed live-run id → resume (checkpoint rehydration, `resumed` note); echoed live-session id → continuation with a NEW server-minted run id in the `run_id` frame and a linked `agent.runs` row; terminal-run-over-live-session → continuation; cross-user / unknown / soft-deleted → fresh session; disconnect-at-terminator still persists the per-turn terminal write. Verify: new tests fail, then pass after 2.2.
- [ ] 2.2 Refactor `POST /agent/run` in `main.py`: continuation branch calls `create_run(user_id, session_id=echoed)` and sets `state["session_id"]`; chat-history call sites (`create_session`, `touch_session`, `record_message`, title generation) key on `session_id`; terminal persistence block (`mark_completed`/`mark_failed`) applies to every turn. Verify: 2.1 green, full `pytest tests/ -q` green.
- [ ] 2.3 Confirm no regression in sweeper/retention behavior for per-turn rows: existing `agent-run-sweeper` tests pass unchanged, and one new test reaps a session-linked row stalled past the heartbeat deadline. Verify: `pytest tests/test_run_sweeper.py -q`.
- [ ] 2.4 Checkpoint-volume guard: confirm the retention GC deletes langGraph threads for terminal per-turn runs within the configured window; add one test asserting thread cleanup is scheduled for a completed continuation turn. Verify: `pytest tests/ -q -k retention`.

## 3. Approvals: runs-row-first ownership

- [ ] 3.1 Update `approvals_api` tests: ownership resolves via the per-turn `agent.runs` row without touching the session store (stub asserts no session lookup); legacy path (no runs row + owned live session) still records; fail-closed cases unchanged. Verify: `pytest tests/test_approvals_api.py -q`.
- [ ] 3.2 Simplify `_run_ownership_verified` documentation/flow to state runs-row-first with legacy fallback (code change only if flow ordering differs); behavior-identical. Verify: 3.1 suite green.

## 4. End-to-end verification (dev stack)

- [ ] 4.1 Side-stack e2e without vLLM: side agent on a spare port against dev Postgres/Redis — create session via `POST /agent/sessions`, drive a decision for a seeded per-turn intent, assert `{"recorded": true}` and one `agent.runs` row with `session_id` linked; foreign-user decision still 404. Verify: scripted checks in the session transcript.
- [ ] 4.2 Live e2e per `test:e2e:live` conventions (`--workers=1`, side stack): new-chat → message that raises `confirm_request` → allow → feed created, no error toast, and the `run_id` frame differs from the echoed session id. Verify: spec passes against the side stack.
- [ ] 4.3 Cross-version wire check: run the updated SPA against a pre-change agent binary — resume and new-chat flows degrade to today's behavior (no thread chopping). Verify: manual pass documented in the change notes, or `@live` spec marked accordingly.

## 5. Frontend: resume with the streamed run id

- [ ] 5.1 Coordinate file ownership with the in-flight chat-history work stream (confirm `chatStore.ts`/`chatService.ts` are free or rebase point agreed). Verify: agreement noted in the session transcript before editing.
- [ ] 5.2 `chatStore` stores the last `onRunId` value per open panel and the resume path sends it; thread hint stays the session id; vitest for both. Verify: `cd frontend && npm test`.
- [ ] 5.3 Playwright e2e: mid-stream reload resumes the same thread (no duplicate session row); new-chat continuation keeps one thread. Verify: `npm run test:e2e` green (agent-free specs), live spec from 4.2 re-run.

## 6. Docs and bookkeeping

- [ ] 6.1 Docs: update the agent README/AgentStack run-id ↔ session-id contract section (per memory: grep the previous feature's identifiers to find every surface — `run_id`, `persist_run_state`, continuation) in both languages. Verify: `grep -rn "persist_run_state\|continuation" docs/ backend/agent_py/README.md` shows no stale claim.
- [ ] 6.2 Backfill script for historical rows (`UPDATE agent.runs r SET session_id = r.id FROM chat_sessions c WHERE c.id = r.id AND r.session_id IS NULL`) committed under `backend/agent_py/scripts/` with an idempotency note; rehearsed on the dev DB. Verify: running it twice changes zero rows the second time.
- [ ] 6.3 Record the deploy-order constraint (backend before frontend, design D6) in the change notes; if any production gate surfaces during implementation, create `openspec/changes/agent-per-turn-run-ids-rollout/` instead of adding it here. Verify: notes present in this change's directory.
