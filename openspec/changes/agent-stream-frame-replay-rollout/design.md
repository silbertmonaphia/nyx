# Design

## Context

The implementation change owns the mechanism; this change owns the clocks and the deployed proof. Detach (the previous change) already proved a run survives its connection; this one must prove the **answer** survives it too — a property that cannot be asserted without a real severed socket and a live model.

The buffer is in-process (see the implementation's design D1): the new runtime cost is memory inside the agent process, not a network round-trip and not a shared-store dependency. That changes what the observation window watches.

## Gate order and rationale

The staging drill comes first: it is the only place the gap-free claim is observable end to end. The observation window follows the deploy because in-process buffering shows up only when several runs are live at once — memory per run is small but the aggregate is what matters, and the flag is the only lever if it misbehaves. Rollback floor: the flag off restores the pre-change wire exactly, and the SPA ladder already handles id-less streams.

## Dependencies

- `openspec/changes/agent-stream-frame-replay` merged and deployed.
- The archived `agent-stream-resilience` behavior in production (detach on) — the drill exercises both.
- Deploy doc / runbook cited from `agent-production-rollout`, not duplicated.
