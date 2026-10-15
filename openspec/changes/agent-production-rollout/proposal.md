# Proposal: Agent production rollout

## Why

Three changes shipped the agent capability end-to-end:

- `agent-loop` — Go infra: provider seam (tool calls + streaming `Delta` union), tool registry, MCP shim, frontend SSE parser for the new frame kinds.
- `agent-langgraph` — Python LangGraph orchestrator replacing the Go agent package, plus the Go chat SSE proxy and the Go RAG bridge endpoint.
- `agent-run-state` — Postgres-backed run persistence (langGraph `AsyncPostgresSaver` + `agent.runs` metadata), retention cron, resume semantics.

All three are **code-complete**: tests pass (Python + Go + frontend + e2e), CI gates are wired, deploy docs and a rollback runbook are written. What remains in each change's task list are **production-rollout gates** — manual smokes against a live stack, 1-week CI green windows, env flips in production, and a staging rollback drill. None of these are engineering work; they are wait-for-production and wait-for-staging gates that would otherwise keep three finished implementation changes "active" in `openspec/changes/` indefinitely.

This change consolidates those gates into one workstream so the three implementation changes archive cleanly, and the rollout is tracked as a single unit (it always was one unit — the gates are shared: one flip of `AGENT_ENABLED=true` closes a task in each of the three changes).

## What Changes

- **MODIFIED** (task split only, no code): the production-gate tasks in `agent-loop` (5.2, 6.1–6.4), `agent-langgraph` (11.2, 14.1–14.4), and `agent-run-state` (9.6, 13.1, 13.3, 13.4) move into this change. The implementation changes keep their completed tasks and archive.
- **NEW** this change's task list: dev-stack smokes, 1-week CI green, `LLM_FALLBACK_TOOLS_ENABLED` flip, `AGENT_ENABLED` production flip, staging rollback drill, and final archive.
- **NO spec deltas**: this is an ops/rollout change. All behavior is already specced in the three implementation changes. `.openspec.yaml` carries `skip_specs: true`.

## Impact

- Affected specs: none (no deltas).
- Affected code: none. The only artifacts are `tasks.md` restructuring in the three implementation changes (done as part of moving the gates) plus this change's own files.
- References: `docs/agent-deployment.md` (rollout order + verification), `docs/runbooks/agent-run-state-rollback.md` (rollback drill procedure).

## Gates covered (consolidated)

| Gate | Source tasks | Where it sits below |
| --- | --- | --- |
| Manual chat smoke (local stack) | agent-loop 5.2, agent-langgraph 11.2 | §1.1, §1.2 |
| `make up` full-stack smoke (agent-migrate) | agent-run-state 9.6 | §1.3 |
| 1-week CI green | agent-loop 6.1, agent-langgraph 14.1, agent-run-state 13.1 | §2.1 |
| `LLM_FALLBACK_TOOLS_ENABLED=true` flip + canary | agent-loop 6.2 | §2.2 |
| `AGENT_ENABLED=true` production flip | agent-loop 6.3, agent-langgraph 14.2 | §2.3 |
| Rollback drill (staging) | agent-loop 6.4, agent-langgraph 14.3, agent-run-state 13.3 | §2.4 |
| Archive | agent-langgraph 14.4, agent-run-state 13.4 | §2.5 |
