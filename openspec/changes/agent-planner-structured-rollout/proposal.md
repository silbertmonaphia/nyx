# Proposal: agent-planner-structured rollout

## Why

`agent-planner-structured` (planner structured output + bounded retry) is code-complete: implementation, unit/parity tests, docs, and external review all landed and green in dev/CI. What remains are gates that need environments and clocks the implementer does not control — a live vLLM probe, live-provider smokes, and a browser e2e run against the full dev stack. Per the rollout-split convention (`openspec/config.yaml`), those move here so the implementation change can archive cleanly.

## What Changes

- **NEW** (this change): live-provider gates — vLLM `response_format` json_schema probe, structured-path and legacy-path curl smokes, the Playwright run of `frontend/tests/e2e/chat-agent-planner.spec.ts` (committed with the implementation change), and the malformed-planner UI verification.
- **MODIFIED** (task split only, no code): tasks 1.2, 6.1, 6.2, and the e2e-execution half of 7.x move out of `agent-planner-structured`.
- **NO spec deltas**: rollout has no spec-level behavior (`skip_specs: true` in `.openspec.yaml`).

## Impact

- Affected specs: none. Affected code: none.
- The structured planner is **default-on** (`PYTHON_AGENT_PLANNER_STRUCTURED_ENABLED=true`): the vLLM probe (§1.1) decides whether that default is safe for the deployed provider before any rollout. A failing probe means either the deployment flag or the compose parser config changes — the decision is recorded here, and if the shipped default itself must change, that lands as a follow-up commit on the implementation change before its archive.
- References: `openspec/changes/agent-planner-structured/`, `backend/agent_py/README.md` (planner section), `design.md` in this change (gate methodology), `openspec/changes/agent-production-rollout/` (the whole-capability rollout this feeds into — its §2.1 one-week CI-green gate covers this change's suites; its §2.2–§2.4 env flips are where this change's default ultimately matters).
