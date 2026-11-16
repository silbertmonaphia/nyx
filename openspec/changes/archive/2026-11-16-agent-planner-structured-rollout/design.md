# Design

## Context

See `proposal.md`. The implementation change (`agent-planner-structured`) is code-complete and review-approved; this change holds only environment-gated verification. Methodology below so an operator can run each gate without re-deriving it. All planner behaviour is specified in `openspec/changes/agent-planner-structured/specs/agent-planner-structured/spec.md`.

## Gates and commands

### vLLM `response_format` probe (decides the default)

- Stack: `make up --profile vllm` (Qwen3-Instruct + hermes tool parser per `docker-compose.yml`), then `make dev-agent` with `PYTHON_AGENT_LLM_BASE_URL` pointing at vLLM.
- What is exercised: the planner's bound chain sends `response_format={"type":"json_schema","json_schema":{"name":"Plan","schema":{...},"strict":false}}`; the provider must constrain its output to that schema.
- **Pass**: the SSE stream contains a `plan` frame with ≥1 step and no `planner_failed`.
- **Fail** (parser ignores `response_format`): the planner exhausts its retry and emits `plan_invalid`, with `agent_planner_parse_failures_total{retry="exhausted"}` climbing. Decision: set `PYTHON_AGENT_PLANNER_STRUCTURED_ENABLED=false` for that deployment, or fix the compose parser config — record which, in this change's tasks.

### Curl smokes (both paths)

- Structured (default flag): agent up, then
  `curl -N -X POST localhost:9000/agent/run -H 'X-Nyx-User-Id: 1' -H 'Content-Type: application/json' -d '{"messages":[{"role":"user","content":"hi"}]}'`
  Pass: frames include `run_id`, `note`, `plan` (≥1 step), `done`, `terminator`.
- Legacy: restart the agent with `PYTHON_AGENT_PLANNER_STRUCTURED_ENABLED=false`, repeat. Pass: a `plan` frame. Confirms the escape hatch end-to-end.

### Playwright run

- Spec: `frontend/tests/e2e/chat-agent-planner.spec.ts` (committed in the implementation change; auto-collected via `playwright.config.ts` `testDir: './tests/e2e'`).
- Stack: `make up` + `make dev` (backend :8080, frontend :5173, agent :9000, LLM provider reachable).
- Run: `cd frontend && npm run test:e2e -- tests/e2e/chat-agent-planner.spec.ts`.
- Flake posture: the spec drives the real planner model; a small model may decline the tool step for the prompt. A red run here is a finding to record verbatim (the plan-shape contract is what the gate verifies) — loosen the assertion only with that evidence recorded.

### Malformed-planner UI verification

- Intent: the user-visible chat must not crash when the planner needs the retry path.
- Unit-level coverage already exists (`test_retry_succeeds_after_validation_failure`; node-level terminal-frames test). The browser variant needs a controllable provider stub (reject once, then succeed); if no stub can be stood up, record that the unit-level coverage is the accepted substitute and why.

## Risks

- Probe failure changes the shipped default → the decision must land before the implementation change archives. → Mitigation: §1.1 runs before §3; decision recorded in this change and, if the default changes, in the implementation change.
- e2e flake from model nondeterminism → explicit prompt, single focused spec, recorded evidence required to loosen.

## Non-goals

- No new code. No production env flips or canary runs (those live in `agent-production-rollout`).
