# Tasks

Rollout gates for `openspec/changes/agent-stream-frame-replay` (ships default-on — the deploy is the rollout). PM/operator-owned; the implementer runs the gates, records evidence, and leaves this change active. Classification: every task below needs a deployed environment or a waiting window — none belongs in the implementation change.

- [ ] 7.1 Staging gap-free drill (before the production deploy merges): on the default-on build, cut a live run's client mid-answer, reconnect, and assert the rendered answer is identical to the same prompt's uninterrupted answer; repeat with `PYTHON_AGENT_FRAME_REPLAY_ENABLED=false` and confirm the old step-granular behavior returns. Evidence linked here.
- [ ] 7.2 Green window: implementation change merged and main/mvp pipelines green for ≥ 1 week with ids on the wire (no proxy or client regressions attributable to the added `id:` field).
- [ ] 7.3 Production deploy of the default-on build; confirm boot validation accepts the deployed env (buffer max default).
- [ ] 7.4 Post-deploy observation (≥ 48 h): per-process RSS for the agent as concurrent runs peak (the buffer is in-process, so memory is the new cost surface), and any `replay_truncated` frequency; anomalies → set `PYTHON_AGENT_FRAME_REPLAY_ENABLED=false` and reopen here.
- [ ] 7.5 Rollback rehearsal (staging): flip the flag to `false` mid-traffic, verify streams continue id-less and a reconnect falls back to the step projection, then restore.
- [ ] 7.6 Archive: PM archives this change once 7.1–7.5 hold; the implementation change archives independently of it.
