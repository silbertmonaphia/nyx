# Proposal

## Why

`openspec/changes/agent-stream-frame-replay` ships default-on (`PYTHON_AGENT_FRAME_REPLAY_ENABLED=true`; the flag is a kill switch). Unlike a pure code change, it adds a **new write on every streamed frame** to Redis — a latency and memory surface that only a deployed stack can measure, and a replay guarantee that only a real disconnect can prove.

## What Changes

- A staging drill that proves the gap-free property end to end (cut mid-answer, reconnect, assert the rendered answer equals the uninterrupted one).
- An observation window on Redis memory and per-frame write latency after the deploy.
- Rollback = set the flag to `false`: frames lose their ids, the SPA ladder falls back to the step projection, and the previous change's behavior returns intact.

Not changing: no schema, no deploy topology, no frontend release gate (an old SPA build ignores `id:` and behaves exactly as before).

## Capabilities

None — rollout has no spec-level behavior (`skip_specs: true`).

## Impact

- Deployed agent + Redis only.
- Gates cite the deploy doc and runbook rather than duplicating them (see `agent-production-rollout` for the model).
- Archived by the PM/operator after the gates complete; the implementer records evidence and leaves this change active.
