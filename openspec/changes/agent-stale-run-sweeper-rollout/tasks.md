# Tasks

> Rollout gates split out of `agent-stale-run-sweeper` per the rollout-split convention (`openspec/config.yaml`). The implementation change is code-complete and review-approved by the time this change is reached; these gates need a running stack, a real crash, and clocks. PM-owned: do not archive from a dev session. Behaviour spec: `openspec/changes/agent-stale-run-sweeper/specs/agent-run-sweeper/spec.md`. Deploy-doc reference: `docs/agent-deployment.md`.

## 1. Dev-stack live smokes (local stack)

- [ ] 1.1 Kill-9 reap smoke — full stack up (`make up` + `make dev`; side instance on spare ports if the operator's stack is running — never restart theirs). Start a chat that yields a multi-step run, `kill -9` the agent process mid-run, restart the agent with a short `PYTHON_AGENT_STALE_RUN_TIMEOUT_SECONDS` override (e.g. 60). Verify: within timeout + sweep interval the row flips to `status='failed'`, `error_code='stale_run'`; `curl :9001/metrics | grep agent_runs_swept_total` ticks; the run's `langgraph.checkpoint_*` rows are **still present** at reap (`SELECT count(*) ... WHERE thread_id = '<run_id>'` > 0 — the sweeper preserves them; retention collects them later). Record the transcript.
- [ ] 1.2 Resume-window smoke — crash a run as in 1.1. Before the (short) timeout, `POST /agent/run` echoing the `run_id`: stream shows `note{kind:"resumed"}` and `last_heartbeat_at` refreshes. After the timeout plus one sweep, echo the same `run_id`: stream shows `note{kind:"fresh_run"}`. Record both.
- [ ] 1.3 Retention GC smoke — backdate `ended_at` past the retention window (SQL) for one completed run AND one previously reaped run (`error_code='stale_run'`), note their checkpoint row counts, run one retention iteration (short interval override). Verify: both metadata rows are deleted AND both runs' `langgraph.checkpoint_*` rows are gone; `agent_run_retention_deleted_total` ticks. Record.

## 2. Production gates

- [ ] 2.1 Post-deploy observation — after the deploy carrying this change, watch `agent_runs_swept_total` for ≥ 1 week. Expected: zero or only crash-shaped ticks (no reaps while the service is healthy), and `langgraph.checkpoint_*` row growth flattens versus before. Record the counter and the table growth numbers.
- [ ] 2.2 Rollback drill — set `PYTHON_AGENT_SWEEP_ENABLED=false` and restart the agent: verify no further reaps (a seeded stale row stays `running`) while heartbeat writes continue; flip back and verify reaping resumes. Record. No migration rollback is needed — the column stays.
- [ ] 2.3 Deploy notes — update `docs/agent-deployment.md`: the four new env vars + defaults, the `stale_run` error-code meaning, the resume-window semantics, and the rollback path (`PYTHON_AGENT_SWEEP_ENABLED=false`). Verify: the documented defaults match the shipped config.

## 3. Archive

- [ ] 3.1 After §1–§2 are checked: archive `agent-stale-run-sweeper` and this change (PM/operator only — the implementer leaves this unchecked). Verify `openspec validate --archived` passes post-archive.

## References

- `openspec/changes/agent-stale-run-sweeper/` — implementation change (proposal, design, specs, tasks).
- `docs/agent-deployment.md` — rollout order, env vars, verification queries.
- `openspec/changes/agent-tool-dispatch-retry-rollout/` — sibling model this change mirrors.
