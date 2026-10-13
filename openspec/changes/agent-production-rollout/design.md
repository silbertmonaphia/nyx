# Design

## Context

See `proposal.md` for why this change exists. In short: three implementation changes (`agent-loop`, `agent-langgraph`, `agent-run-state`) are code-complete; their remaining unchecked tasks are production-rollout gates (live smokes, wait periods, env flips, a staging drill) that are shared across all three and would otherwise keep the changes "active" indefinitely.

## Decisions

### 1. One rollout change, not three §"Rollout" sections

The alternative — leaving the gates in each implementation change — was the status quo and it failed for a practical reason: the gates are not independent. `AGENT_ENABLED=true` is one production env flip that closes a task in `agent-loop` AND `agent-langgraph` AND (via `PYTHON_AGENT_DATABASE_URL`) `agent-run-state`. Splitting one operator action across three task lists makes "is the rollout done?" unanswerable from any single change. Consolidating also lets the implementation changes archive at implementation-complete, which is what their spec intent was ("archive once all tasks are checked" assumed all tasks were implementation tasks — the rollout gates were bolted on later).

### 2. No spec deltas (`skip_specs: true`)

This change adds no behavior. Every observable contract (SSE frames, error codes, env keys, resume semantics, retention) is already specced in the three implementation changes and synced into `openspec/specs/`. Writing delta specs here would duplicate them or invent a fake capability ("production-rollout") that has no runtime surface. `.openspec.yaml` carries `skip_specs: true` so `openspec validate` accepts the zero-delta change.

### 3. Task numbers are renumbered, not preserved

The moved tasks get new numbers (1.1–1.3, 2.1–2.5) and the originating changes link back with "(moved) → see agent-production-rollout/§X.Y". Preserving the old numbers across two files (e.g. "task 14.3" here referring to agent-langgraph's numbering) would make cross-references ambiguous. The mapping table in `proposal.md` keeps the provenance.

### 4. Rollout order is defined in the deploy doc, referenced here

The canonical order lives in `docs/agent-deployment.md` (agent-migrate → verify → roll replicas → flip Go env). This change's tasks reference it instead of restating it — same reasoning as the runbook reference in 2.4: one authoritative procedure, linked, not duplicated.

## Risks

- **Archive friction**: `openspec archive` may warn on the "(moved)" unchecked tasks remaining in the three implementation changes. If the CLI enforces "all tasks checked", the fallback is to delete the moved-task lines from the implementation changes entirely (their provenance lives in this change's proposal table) rather than leaving `[ ]` stubs. Try archive first; adjust if it refuses.
- **Stale rollout change**: if production never flips `AGENT_ENABLED=true`, this change sits open forever. Acceptable — the change is a genuine pending operational decision, and its open state is the honest signal. If the direction changes (agent feature canceled), the change closes as abandoned rather than done.
