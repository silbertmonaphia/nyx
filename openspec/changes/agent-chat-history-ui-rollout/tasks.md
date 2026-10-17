# Tasks

> Rollout gates split out of `agent-chat-history-ui` per the rollout-split convention (`openspec/config.yaml`). The implementation change is code-complete and review-approved by the time this change is reached; these gates need a deployed stack, a live browser, and load data. PM-owned: do not archive from a dev session. Behaviour specs: `openspec/specs/agent-chat-sessions-api/spec.md` + `openspec/specs/chat-history-sidebar/spec.md` once synced. Deploy-doc reference: `docs/agent-deployment.md`.

## 1. Dev-stack live smokes (local stack)

- [ ] 1.1 REST proxy curl smoke — full stack up (`make up` + `make dev`; side agent instance on a spare port if the operator's stack is running — never restart theirs). With a valid JWT: `curl -H "Authorization: Bearer <t>" localhost:8080/api/chat/sessions` → `{"sessions": [...]}`; `POST /api/chat/sessions` → new uuid; `GET /api/chat/sessions/<id>/messages` after one chat → both messages; `DELETE /api/chat/sessions/<id>` → subsequent list omits it and messages 404. Also verify an unauthenticated call → 401 and a call with `AGENT_ENABLED=false` → 503. Record.
- [ ] 1.2 Run-id echo smoke — drive a chat through the SPA with an active session; verify the proxied `/agent/run` body carried `run_id=<active session>` (agent logs / wire capture) and the run appended to that session (`SELECT count(*) FROM agent.chat_messages WHERE session_id=...` grew). Then verify an omitted run_id still starts a fresh session. Record.
- [ ] 1.3 Pagination load check — seed ≥ 25 sessions for one user (SQL insert is fine). In the browser: sidebar shows 10, scroll loads exactly 10 more, final page sets `next_cursor=null` and the sentinel disappears; no duplicated rows; the list query stays under the p95 latency budget (check the OTel span `agent.sessions.list` in Jaeger). Record.
- [ ] 1.4 Cross-user isolation spot-check — two users in the same browser profile (or two profiles): user A's session id in user B's call → 404 through the full SPA → Go → agent chain. Record.

## 2. Browser e2e

- [ ] 2.1 Playwright run on the merged change — `cd frontend && npm run test:e2e -- tests/e2e/chat-history-sidebar.spec.ts` against the live stack. Verify green (3-session flow, reload restore, +New chat, oldest-session replay). If red, record the failure verbatim here (model nondeterminism is a finding, not a reason to loosen silently).
- [ ] 2.2 Mobile viewport run — run the same spec (or a viewport variant) at 600×900: drawer hidden by default, hamburger opens it, session click closes it, replay renders. Record.
- [ ] 2.3 Session-idle boundary — with `PYTHON_AGENT_SESSION_IDLE_MIN=1`, verify a chat after >1 min of inactivity opens a fresh session while a follow-up within the window continues the active one. Record the two `run_id` values.

## 3. Production gates

- [ ] 3.1 1-week CI green — backend (`go test ./internal/chat/...` + full suite) and frontend (vitest + the new e2e spec in any CI job that runs Playwright) green in CI for ≥ 1 week before any production flip. Wait-and-record.
- [ ] 3.2 Staging flip — deploy with the sidebar enabled; run the §1.1 + §1.3 + §2.1 flows against staging; verify the Go proxy's 502/503 envelopes surface correctly under a forced agent outage (stop the agent container briefly). Record.
- [ ] 3.3 Rollback drill — flip the SPA flag `VITE_CHAT_HISTORY_SIDEBAR_ENABLED=false`, rebuild, verify the UI reverts to the live-stream-only experience with zero errors and the REST routes remain harmlessly idle. Flip back. Record.
- [ ] 3.4 Deploy notes — update `docs/agent-deployment.md`: the new SPA flag, the five REST routes and their 502/503 semantics, the two new agent env vars, and the rollback path. Verify documented defaults match shipped config.

## 4. Archive

- [ ] 4.1 After §1–§3 are checked: archive this change (PM/operator only — the implementer leaves this unchecked). Verify `openspec validate --archived` passes post-archive.

## References

- `openspec/changes/agent-chat-history-ui/` — implementation change (proposal, design, specs, tasks).
- `openspec/specs/agent-chat-sessions-api/spec.md` + `openspec/specs/chat-history-sidebar/spec.md` — canonical behaviour specs synced at archive.
- `docs/agent-deployment.md` — rollout order, env vars, verification queries.
- `openspec/changes/agent-chat-history-storage-rollout/tasks.md` — the storage-layer gates that must precede this UI rollout.
- `docs_agent/AgentMemoryRoadmap.md` — Slice 2 of the 4-slice plan; this rollout is its production tail.
