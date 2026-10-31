# Proposal

## Why

`openspec/changes/agent-stream-resilience` ships detach **default-on** (`PYTHON_AGENT_STREAM_DETACH_ENABLED=true` is the built-in default; the env var is a kill switch). That means the behavior change IS the deployment: the first production deploy of the implementation puts detached runs live, with no env flip in between. The gates below exist because the change lands on the agent's live daily path and its failure signatures (runs stuck `running`, budget deltas) lag actual breakage.

## What Changes

- Staging disconnect drill against the default-on build before the production deploy merges.
- A bounded observation window after the deploy: detached-cap `failed` runs, per-user budget deltas, stuck-`running` counts.
- Rollback = `PYTHON_AGENT_STREAM_DETACH_ENABLED=false` (a kill switch, not a revert): the SPA ladder degrades to its resume rung automatically. Keep-alive config and the re-attach route are wire-safe in every state.

Not changing: no schema, no deploy topology, no frontend release gate.

## Capabilities

None — rollout has no spec-level behavior (`skip_specs: true`).

## Impact

- Deployed agent only; the cap default (600 s) keeps an unconfigured upgrade booting.
- Gates cite the deploy doc and runbook rather than duplicating them (see `agent-production-rollout` for the model).
- Archived by the PM/operator after the gates complete; the implementer records evidence and leaves this change active.
