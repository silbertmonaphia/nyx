# Tasks

Rollout gates for `openspec/changes/agent-stream-resilience` (detach ships default-on — the deploy is the rollout). PM/operator-owned; the implementer runs the gates, records evidence, and leaves this change active. Classification: every task below needs a deployed environment or a waiting window — none belongs in the implementation change.

- [ ] 6.1 Staging disconnect drill (before the production deploy merges): on the default-on build, cut a live run's client mid-stream, verify (a) the run completes server-side with terminal writes, (b) a re-attach streams replay + live frames to completion, (c) cap expiry marks the run `failed` with the typed code, (d) `PYTHON_AGENT_STREAM_DETACH_ENABLED=false` staging still recovers via the resume rung. Evidence linked here.
- [ ] 6.2 Green window: implementation change merged and main/mvp pipelines green for ≥ 1 week with the re-attach route and keepalive config live in staging (no proxy idle-timeout regressions attributable to the new route).
- [ ] 6.3 Production deploy of the default-on build; confirm boot validation accepts the deployed env (unset keys = defaults) and the cap default is sane for the deployed Go `LLM_MAX_STREAM_DURATION`.
- [ ] 6.4 Post-deploy observation (≥ 48 h): detached-cap `failed` runs, per-user budget deltas, and stuck-`running` run counts watched; anomalies → set `PYTHON_AGENT_STREAM_DETACH_ENABLED=false` and reopen here.
- [ ] 6.5 Rollback rehearsal (staging): set the flag to `false` mid-traffic, verify live detached runs drain to terminal and new runs bind to request scope as before, then restore.
- [ ] 6.6 Archive: PM archives this change once 6.1–6.5 hold; the implementation change archives independently of it.
