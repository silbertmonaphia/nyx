# Proposal

## Why

The agent's mutating-tool safety is ask-only today: every non-read-only registry call surfaces a `confirm_request` card and waits for the user. An injected or hallucinated dangerous call (fetched web content is untrusted input in the same context that emits tool calls) therefore always *reaches the user* as an approval prompt — alert fatigue, and one careless click from execution. The operator has no way to flat-deny a tool or argument pattern; the confirmation gate is name-level only and explicitly skips agent-native web tools. The promptfoo security suite measures injection outcomes but nothing enforces policy — evals assert, they don't stop.

## What Changes

- New deny-policy layer evaluated in the dispatch funnel **before** the confirmation gate. Policy (JSON file) is loaded, validated, and snapshotted at agent startup; an invalid or unreadable policy fails the boot (same posture as `JWT_SECRET` validation), never a silent allow-all.
- Rules match tool name + canonical-serialized args (reusing the funnel's `_canonical_args` serialization) with glob patterns; first match wins; no match → allow and the existing gates apply unchanged.
- Deny is a **closed outcome**, not a failure class: static safe `blocked by policy` text feeds the `tool_result{ok:false}` frame and the react-loop `ToolMessage` — same shape as confirmation denials; no retry budget, no re-prompt, no confirm card.
- Applies uniformly to agent-native (local) tools and MCP registry tools; registry-outage runs keep failing closed for non-local names (unchanged) while policy rules still evaluate.
- Per-decision audit: structured log line + prometheus counter (`tool`, `decision`, `rule_id`) — counts and identifiers only, never arg content (same stance as `audit.py` payload summaries).
- Default off: unset `AGENT_TOOL_POLICY_PATH` → dispatch behavior is byte-identical to today.
- Security eval suite gains policy-gate cases: an injected "call the destructive tool" prompt must produce a blocked tool result with the delivery marker present (anti-vacuity convention).

Not changing: confirmation-gate semantics (`agent-mcp-tools` / `chat-approval-card`), the retry taxonomy, the SSE frame set (no new kinds), the Go registry, the SPA.

## Capabilities

### New Capabilities

- `agent-tool-policy`: operator-configured deny rules for agent tool calls — startup snapshot and fail-closed boot validation, name+args glob matching with first-match-wins ordering, deny-before-confirm evaluation in the dispatch funnel, static model-facing denial text, per-decision audit, default-off backward compatibility.

### Modified Capabilities

- none — a policy denial is a closed outcome that never dispatches, so "mutating registry calls pass the confirmation gate before dispatch" remains true; no existing requirement changes.

## Impact

- `backend/agent_py/agent/`: new `tool_policy.py` (load/validate/match); `executor.py` `_dispatch_with_retries` (policy check ahead of the confirmation-gate block); `config.py` (`AGENT_TOOL_POLICY_PATH`); `main.py` (startup load + snapshot wiring).
- `evals/`: new cases in the security suite loader.
- No DB migration, no new dependency (stdlib `json` + `fnmatch`), no SSE/wire change, no frontend or Go change.
- Rollout gates: none foreseen — the change is dev/CI-completable end to end (implementation + unit tests + eval cases run against the bundled stub), so per the rollout-split classification test there is **no sibling rollout change**.
