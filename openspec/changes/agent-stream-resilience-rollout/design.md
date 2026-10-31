# Design

## Context

The implementation change ships the mechanism with detach on by default; this change owns the clocks. Unlike the model rollout sibling (`agent-production-rollout`), there is no env-flip gate — the default is the rollout — so the gates are drill → green window → deploy → observe, with the kill switch as the standing rollback.

## Gate order and rationale

The staging drill precedes the deploy because the supervisor refactor touches the run hot path: the byte-for-byte regression suite guards the wire, but only a drill shows a run surviving a real severed socket with terminal writes landing. The observation window exists because detach's failure modes are lagging indicators (a stuck budget or a `failed`-by-cap run shows up minutes after the disconnect, not at deploy time). Rollback floor: flag off restores pre-change disconnect semantics and the ladder falls back to its resume rung — no worse than today.

## Dependencies

- `openspec/changes/agent-stream-resilience` (implementation) merged and deployed.
- Deploy doc / runbook: the production agent env surface documented in `README.md` (agent section) and the ops runbook linked from `agent-production-rollout` — cited, not duplicated.
