# Tasks

> Rollout gates split out of `agent-user-preferences` per the rollout-split convention (`openspec/config.yaml`). The implementation change is code-complete and review-approved by the time this change is reached; these gates need a deployed stack, an observation window, and real cost data. PM-owned: do not archive from a dev session. Behaviour spec: `openspec/specs/agent-user-prefs/spec.md` (once synced). Deploy-doc reference: `docs/agent-deployment.md`.

## 1. Dev-stack live smokes (local stack)

- [ ] 1.1 Extraction smoke — full stack up with the host vLLM (`make dev-vllm-main`). Send a preference-bearing message ("我喜欢鱿鱼丝，也喜欢ACG") through the chat UI. Verify: within a few seconds `SELECT * FROM agent.chat_user_prefs WHERE user_id=<u>` shows ≥ 1 row with a sensible key/value; the chat itself completed normally (extraction ran after `done`; the stream was not delayed). Record the row + timestamps.
- [ ] 1.2 Injection smoke — start a NEW session (or wait past the idle window), ask "推荐一个零食给我". Verify via the Langfuse trace (or a debug log of the planner system prompt) that the prompt carried the preferences block with the 鱿鱼丝/ACG entries; the answer referencing the preference is a recorded observation (model nondeterminism is a finding, not a failure). Record.
- [ ] 1.3 Failure-isolation smoke — stop the LLM provider (or point the extraction model at a dead URL via env), send a preference-bearing message. Verify: the chat still streams to `done` + `terminator` normally; the extraction failure is warn-logged; no prefs row is written; the next chat is unaffected. Record.
- [ ] 1.4 Gate behavior — send a message with no preference signal ("今天天气怎么样"). Verify no extraction LLM call fires (log line absent / no prefs row candidate), confirming the heuristic gate skips it. Record.

## 2. Browser e2e

- [ ] 2.1 Playwright run on the merged change — `cd frontend && npm run test:e2e -- tests/e2e/chat-user-prefs.spec.ts`. The recall leg depends on the live model: record the outcome verbatim; a model that ignores the injected preference is a recorded finding (the spec asserts the memory plumbing, not model obedience, wherever it cannot be made deterministic). Ensure any stub-backed legs are green.

## 3. Production gates

- [ ] 3.1 1-week CI green — agent pytest suites (including the new prefs tests) green in CI for ≥ 1 week before any production flip. Wait-and-record.
- [ ] 3.2 Cost watch — after enabling in staging, watch `agent_tokens_total` for the extraction role and the LLM provider bill delta for ≥ 1 week. Expected increment: bounded by one small extraction call per preference-bearing turn (the gate skips the rest). Confirm within the cost budget; tune the gate heuristic only via a code change (it has no env knob by design). Record the numbers.
- [ ] 3.3 Injection parity check — with zero prefs for the test user, verify the planner system prompt is byte-identical to the pre-injection form (log diff or Langfuse prompt compare). This is the "off-path is unchanged" contract. Record.
- [ ] 3.4 Kill-switch drill in staging — set `PYTHON_AGENT_PREFS_ENABLED=false`, send a preference-bearing message, verify no extraction call fires and no rows are written while chats complete normally; flip back and verify extraction resumes. Record.
- [ ] 3.5 Production flip — after 3.1–3.4, enable in production. Record the flip in the deploy notes (`docs/agent-deployment.md`: the new env vars, the gate heuristic, the kill-switch, the prefs table lifecycle — rows follow the chat-retention window collection story per `agent-chat-history-store`).

## 4. Archive

- [ ] 4.1 After §1–§3 are checked: archive this change (PM/operator only — the implementer leaves this unchecked). Verify `openspec validate --archived` passes post-archive.

## References

- `openspec/changes/agent-user-preferences/` — implementation change (proposal, design, specs, tasks).
- `docs_agent/AgentMemoryRoadmap.md` — Slice 3 of the 4-slice plan.
- `openspec/changes/agent-chat-history-storage-rollout/tasks.md` — sibling model; the prefs table rides the same retention story.
