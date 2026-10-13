# Tasks

> Consolidated production-rollout gates from `agent-loop`, `agent-langgraph`, and `agent-run-state`. The three implementation changes are code-complete and archive independently; this change tracks the cross-cutting gates that could only run against a live/dev/staging stack or after a waiting period.

## 1. Dev-stack smoke (local, before any production flip)

- [ ] 1.1 Manual chat smoke — from `agent-loop` 5.2. Against the local stack (`make up` + `make dev`), trigger a chat that produces a `list_feeds` plan and verify the SSE stream shows `plan` → `step_start` → `tool_call` → `tool_result` → `step_end` → `delta` → `done`.
- [ ] 1.2 Manual Python-agent smoke — from `agent-langgraph` 11.2. `make up`, trigger a chat in the browser; verify the SSE stream contains `plan` → `step_start` → `tool_call` → `tool_result` → `step_end` → `delta` → `done` → `terminator`, and the UI renders the agent-steps disclosure.
- [ ] 1.3 Full-compose smoke — from `agent-run-state` 9.6. `docker compose --profile app up --build -d`; verify `agent-migrate` exits 0 (alembic head applied) and `agent` starts healthy. Two Dockerfile bugs found during the original attempt are already fixed (commit `4b75a96`): missing `alembic.ini`/`migrations/` copy + missing `libpq5`. The remaining assertion is the one-shot exit-0 against a clean rebuild.

## 2. Production gates

- [ ] 2.1 1-week CI green — from `agent-loop` 6.1, `agent-langgraph` 14.1, `agent-run-state` 13.1. Both providers' tool-call integration tests AND the Python agent's pytest suite green in CI for ≥1 week before any production flip. CI already runs the suites (`backend-test` + `agent-alembic-check` jobs); this is a wait-and-record gate.
- [ ] 2.2 Flip `LLM_FALLBACK_TOOLS_ENABLED=true` in production + canary — from `agent-loop` 6.2. After 2.1, verify the per-provider failover paths still emit the same `Delta` shape end-to-end via a canary run.
- [ ] 2.3 Flip `AGENT_ENABLED=true` + `PYTHON_AGENT_URL=https://agent.<prod-domain>` in production — from `agent-loop` 6.3, `agent-langgraph` 14.2. Rollout order per `docs/agent-deployment.md`: agent-migrate → verify alembic head → roll agent replicas → flip Go env. Verify `agent_runs_total{kind,status}`, `agent_steps_total{kind,status}`, `agent_tool_calls_total{tool,ok}`, `agent_tokens_total{role,model}`, and `agent_budget_exhausted_total` are reachable in Prometheus. Document the rollout in deploy notes.
- [ ] 2.4 Rollback drill in staging — from `agent-loop` 6.4, `agent-langgraph` 14.3, `agent-run-state` 13.3. Two drills in one window: (a) set `AGENT_ENABLED=false` and verify `/api/chat` reverts to single-turn behavior with no other changes; (b) drop `agent` + `langgraph` schemas and verify the service re-runs migrations + `saver.setup()` on restart, then a chat lands a fresh `agent.runs` row. Procedure: `docs/runbooks/agent-run-state-rollback.md`. Record the outcome here.
- [ ] 2.5 Archive this change once 2.1–2.4 are checked.

## References

- `docs/agent-deployment.md` — rollout order, env vars, per-step verification queries, common gotchas.
- `docs/runbooks/agent-run-state-rollback.md` — the drop-schemas drill procedure with pass/fail criteria.
- `openspec/changes/agent-loop/`, `agent-langgraph/`, `agent-run-state/` — the archived implementation changes (their tasks.md link back here for the moved items).
