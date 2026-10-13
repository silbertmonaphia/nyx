# Tasks

> Rollout gates split out of `agent-tool-dispatch-retry` per the rollout-split convention (`openspec/config.yaml`). The implementation change is code-complete and review-approved (archived as `openspec/changes/archive/2026-10-13-agent-tool-dispatch-retry/`); these gates need a running provider stack / browser environment. PM-owned: do not archive from a dev session. Methodology: `design.md` in the archived implementation change; behaviour spec: `openspec/specs/agent-tool-retry/spec.md`.

## 1. Dev-stack live smokes (local stack)

- [ ] 1.1 Counter smoke — full stack up. `curl :9001/metrics | grep agent_tool_retries_total` confirms the counter is registered with labels `tool` and `reason`. Drive a chat that triggers a tool-arg retry; the counter increments on `:9001/metrics` for `reason="tool_arg_invalid"`. Then drive a chat that triggers a transient retry (or stop the MCP bridge briefly); the counter increments for `reason="tool_execution_transient"`. Record both results.
- [ ] 1.2 Tool-arg retry curl smoke — `make up` + `make dev` + `make dev-agent`. `curl -N -X POST localhost:9000/agent/run -H 'X-Nyx-User-Id: 1' -H 'Content-Type: application/json' -d '{"messages":[{"role":"user","content":"…something that triggers a registry-rejected tool call…"}]}'`. Verify the SSE stream contains `tool_call` → `tool_result{ok:false, retry_count:1}` → another `tool_call` → final `tool_result{ok:true, retry_count:1}`; counter on `:9001/metrics` ticks.
- [ ] 1.3 Transient-retry curl smoke — with the MCP bridge stopped, repeat the chat that calls a tool. Verify the SSE stream shows exponential-backoff delays between `tool_result{ok:false}` frames (delay visible in frame timestamps), bounded by 5 s per attempt; once the cap is hit, `step_failed` and the run continues. Restart the bridge; a fresh chat succeeds first-try and `retry_count=0`. Counter on `:9001/metrics` ticks for `reason="tool_execution_transient"`.

## 2. Browser e2e

- [x] 2.1 Playwright run — **green 3/3** (2026-10-13, implementation commit `c6caa9e`, agent restarted on the fixed code by the operator): the live-stack test plus both recorded-wire-byte replays passed in Chromium (`retry-recovered` → badge, no banner; `retry-exhausted` → banner with `data-failed-steps="step_1"`). No failures to record.
- [ ] 2.2 Pluggable-stub e2e — from implementation change §6.1. The spec is wired for a controllable provider stub. Drive a chat that emits a malformed tool call on first attempt and a valid one on retry. Assert the `tool_result` row carries the retry metadata, every row reaches a terminal status, no `error` frame, and the final answer is non-empty. If the live stack cannot provide a controllable stub, record that the unit-level coverage (implementation change §3.3, §3.4, §4.2, §4.3) is the accepted substitute and why.

## 3. Production gates

- [ ] 3.1 1-week CI green — both providers' tool-call integration tests AND the Python agent's pytest suite green in CI for ≥ 1 week before any production flip. CI already runs the suites (`backend-test` + `agent-alembic-check` jobs); this is a wait-and-record gate.
- [ ] 3.2 Flip `PYTHON_AGENT_TOOL_RETRY_MAX=2` in production — after §3.1, verify the retry paths still terminate in the same `step_failed` semantics via a canary run. `agent_tool_retries_total{tool, reason}` reachable in Prometheus.
- [ ] 3.3 Document the rollout in deploy notes — the env-var addition, the expected initial counter rates, the rollback path (`PYTHON_AGENT_TOOL_RETRY_MAX=0` disables both retry paths and the executor returns to today's first-attempt behavior).
- [ ] 3.4 Rollback drill in staging — two drills in one window: (a) set `PYTHON_AGENT_TOOL_RETRY_MAX=0` and verify the tool-dispatch path reverts to first-attempt-only with no other changes; (b) leave `PYTHON_AGENT_TOOL_RETRY_MAX=2` and stop the MCP bridge — verify the transient retry path fires, exhausts the budget, terminates the step, and the run continues to the next step. Record the outcome here.

## 4. Archive

- [ ] 4.1 After §1–§3 are checked: archive `agent-tool-dispatch-retry` and this change. Verify `openspec validate --archived` passes post-archive.

## References

- `openspec/changes/archive/2026-10-13-agent-tool-dispatch-retry/` — implementation change (tasks.md, design.md, specs/).
- `openspec/specs/agent-tool-retry/` + `openspec/specs/agent-partial-failure/` — canonical contracts synced at archive.
- `docs/agent-deployment.md` — rollout order, env vars, per-step verification queries, common gotchas.
- `openspec/changes/agent-planner-structured-rollout/` — model for this rollout change's shape (sibling created 2026-10-13).
- `openspec/changes/agent-production-rollout/` — the prod-rollout track; this change shares its production env-flip queue.
