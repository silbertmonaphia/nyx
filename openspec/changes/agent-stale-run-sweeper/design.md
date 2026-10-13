# Design

## Context

See `proposal.md` — Why. Current state that shapes the approach:

- `agent.runs` has no liveness column; `status='running'` today conflates "alive", "crashed", and "deliberately left running by `agent_unavailable`". The resume decision (`main.py`, `RunMetadata.get_run`) treats any non-terminal row as resumable.
- `RetentionCron` (`retention.py`) deletes only `status IN ('completed','failed')` rows; nothing reaps `running` rows. `retention.py`'s comment claiming LangGraph prunes its own checkpoints is false — `adelete_thread` is never called anywhere, so `langgraph.checkpoint_*` grows unbounded for both crashed and completed runs.
- The checkpointer (`AsyncPostgresSaver`) is already built at lifespan startup and stored on `app.state.checkpointer`; the graphs stream through `_stream_agent_run` in `main.py`, whose `finally` owns the terminal metadata write.
- All metadata SQL goes through `RunMetadata` (asyncpg) against the DSN whose `search_path` is `langgraph,agent`; retention's predicates use Postgres `now()` server-side (the single-clock stance this design extends).
- `agent-tool-dispatch-retry` is being landed in parallel; this slice stays off `executor.py` / `types.py` / `tools_client.py` / `state.py` / `frontend/` per the proposal's scope fence.

## Goals / Non-Goals

**Goals:**

- A `running` row means "a live process is heartbeating"; a dead process's row becomes terminal within `sweep_interval + stale_timeout` worst case.
- The crash-resume escape hatch stays available for a bounded, documented window, then resume degrades to today's terminal-row behaviour (`fresh_run`), with no wire change.
- `langgraph.checkpoint_*` is GC'd at a single lifecycle exit — retention, through the framework's supported API — with reaped runs' checkpoints deliberately preserved until then.
- Rollback is a single env flip; no migration rollback required to stop reaping.

**Non-Goals:**

- Killing live processes (we reap the dead, we do not cancel the living).
- A run-history endpoint or UI (separate future slice); `stale_run` needs no new client rendering.
- Recovery machinery for retention-time GC failures beyond logging (see Risks).
- Cleaning up checkpoint threads that never had a metadata row (pre-`0002` dev leftovers) — manual SQL if ever needed.

## Decisions

### D1: Heartbeat = `last_heartbeat_at` column + per-run background task

`agent.runs.last_heartbeat_at timestamptz NOT NULL DEFAULT now()` (alembic `0003`). A background asyncio task, started in `_stream_agent_run` after run-id resolution and cancelled in the generator's `finally`, touches it every `PYTHON_AGENT_HEARTBEAT_INTERVAL_SECONDS` (default 30); the first touch is immediate, so a resumed run re-freshens its row before any step can stall. Touch failures log a warning and never raise.

- *Alternative — touch at step boundaries only*: rejected. A single LLM/RAG call can legitimately run for minutes, forcing the timeout to be sized against worst-case step duration and weakening the reaper. A liveness task is independent of step duration.
- *Alternative — LangGraph-level lease*: rejected. No such primitive exists; building it on the checkpointer couples metadata to framework internals. Column + task is the standard workflow-engine pattern, and Postgres is already a hard dependency.
- *Alternative — one process-wide heartbeat loop over a registry of active runs*: rejected. Per-run task lifecycle is the generator's lifecycle; no registry to leak or synchronize.

### D2: The timeout is the resume window; reap goes to `failed` + `error_code='stale_run'`

`PYTHON_AGENT_STALE_RUN_TIMEOUT_SECONDS` (default 900) is deliberately the same knob as "how long a crashed run stays resumable", because `running` is exactly the resume-eligible state. Reaping transitions to the existing terminal vocabulary — no new status enum value.

- *Alternative — a new `orphaned` status that stays resumable*: rejected. It expands the status space every consumer (`get_run`, list filters, retention predicate) must handle, and "resumable corpse" is the ambiguity this slice removes. Rehydrating a dead run's checkpoint was always best-effort; after the window, a fresh run is the honest outcome.
- *Default rationale*: 15 min ≫ any realistic single-step duration, ≪ "forever", and long enough for a user to re-issue a crashed prompt and still hit resume.
- *Resume-race note*: a resume starting in the instant before a sweep and losing the heartbeat race is still fine — the checkpoint read proceeds and the eventual `mark_completed` is authoritative (D3). The reverse (sweep racing an in-flight resume) requires ≥ timeout of silence first, so it is vanishingly rare.

### D3: Liveness writes are conditional; terminal writes stay authoritative

- `touch_heartbeat`: `UPDATE runs SET last_heartbeat_at=now() WHERE id=$1 AND status='running'` — no-op after terminal is silently OK.
- Reap: `UPDATE runs SET status='failed', error_code='stale_run', ended_at=now() WHERE status='running' AND last_heartbeat_at < now() - ($1 * interval '1 second') RETURNING id` — conditional on both liveness and non-terminality; `RETURNING id` feeds the GC step.
- `mark_completed` / `mark_failed`: unconditional; `mark_completed` additionally sets `error_code=NULL`.
- *Alternative — strict first-writer-wins on terminal writes too*: rejected. It produces the perverse outcome that a genuinely completed run stays labelled `stale_run` because the sweeper fired during a heartbeat stall; the live process's knowledge of its own outcome is strictly better. The terminal-after-reap race is neutralized the other way by the reap's `status='running'` guard.

### D4: Checkpoint GC is single-homed in retention; reaped runs keep their checkpoints

- `retention_delete_terminal` becomes select-then-act: `DELETE ... RETURNING id` (asyncpg `fetch`), and the retention cron calls `checkpointer.adelete_thread(run_id)` per id (may be `None` in metadata-only dev — skip GC then). The sweeper does **not** take the checkpointer and never deletes checkpoint data: a reaped run's thread is collected when retention later deletes its (now terminal) row — the same window as any other terminal run.
- *Rationale (revised during review)*: GC-at-reap would destroy a crashed run's checkpoint ~15 min after death — exactly the state a future replay / "time-machine" feature (resume from an arbitrary earlier `checkpoint_id`) would most plausibly want. Deferring costs only storage for one retention window and keeps the sweeper a pure metadata transition (smaller blast radius, a single GC call-site in the whole service).
- Ordering within retention: `DELETE ... RETURNING id`, then GC per id — the metadata delete stands regardless of GC outcome (failures logged per-run WARN with `run_id`, remaining ids still processed). A persistently failing GC must not silently stall retention's user-visible history deletion. Consequence: a GC failure leaves an orphan (accepted, logged); a documented manual SQL sweep of `langgraph.checkpoint_*` remains the fallback. (GC-first with delete-regardless would be outcome-equivalent — the divergent case doesn't exist — so the simpler delete-then-GC ships.)
- *Alternative — a parallel TTL cron over `langgraph.checkpoint_*` directly* (the other option in `AgentArchitecture.md` §3): rejected. It couples us to LangGraph's internal schema across upgrades; `adelete_thread` is the supported API and cascades across `checkpoints` / `checkpoint_blobs` / `checkpoint_writes`.

### D5: New `sweeper.py` module; retention stays single-purpose

`StaleRunSweeper` mirrors `RetentionCron` structurally (start/stop, never-die loop, OTel span `agent.run.sweep` with `swept_count`, counter `agent_runs_swept_total` registered module-locally like `retention.py`'s). *Alternative — extend RetentionCron*: rejected; distinct cadence (300 s vs 3600 s) and distinct failure signal deserve a distinct module, and retention's blast radius stays small. Lifespan wiring: build when `database_url` set and `sweep_enabled`; store on `app.state.sweeper`; stop alongside retention in teardown.

### D6: Config surface and validators

`heartbeat_interval_seconds=30`, `stale_run_timeout_seconds=900`, `sweep_interval_seconds=300`, `sweep_enabled=True` (env prefix `PYTHON_AGENT_`, matching the existing settings pattern). Validators: heartbeat ≥ 1, sweep interval ≥ 1, timeout ≥ 2× heartbeat — a smaller ratio would let the sweeper reap live runs, so it fails at startup naming both vars. Negative → refuse. `create_run`'s INSERT gains `last_heartbeat_at=now()`; `_RUN_COLUMNS` / `RunSummary` are intentionally unchanged (no reader needs the column yet).

## Risks / Trade-offs

- [False reap of a live run whose heartbeat writes are failing] → reap requires a full timeout of *silence*; heartbeat failures are logged; the ≥ 2× ratio is enforced at startup; and the live process's later terminal write is authoritative (D3), so a false reap self-corrects the moment the run ends.
- [Clock skew] → all predicates compare against Postgres `now()` server-side, never Python timestamps — same stance as retention.
- [Crash-resume escape hatch ends after the window] → documented semantics (spec: "The sweep threshold bounds the resume window"), configurable, and resume inside the window is byte-for-byte today's behaviour.
- [Checkpoint orphan when `adelete_thread` fails at retention time] → logged at WARN with `run_id`; a direct-SQL sweep of `langgraph.checkpoint_*` remains the documented manual fallback (D4).
- [Heartbeat UPDATE load] → one UPDATE per active run per interval; negligible at current concurrency, revisit (batch / longer interval) if run concurrency grows by orders of magnitude.
- [Generator cancelled without the `finally` running cleanly] → heartbeat task is cancelled in the generator's `finally` (which runs on close/GC); tests assert no pending-task warnings.

## Migration Plan

1. Alembic `0003_add_run_heartbeat`: `op.add_column('runs', sa.Column('last_heartbeat_at', TIMESTAMP(timezone=True), nullable=False, server_default=sa.text('now()')), schema='agent')`; downgrade drops the column. Applied through the existing boot path (`_dev_upgrade_hook` in dev, `agent-migrate` one-shot in compose). Pre-existing `running` rows pick up `now()` and get their natural first timeout window before any reap — no backfill script needed.
2. Ship the service with defaults (sweep enabled, 30 s / 900 s / 300 s). No env change required to deploy.
3. Rollback: `PYTHON_AGENT_SWEEP_ENABLED=false` stops reaping immediately; heartbeat writes are inert metadata and may stay. The column stays (harmless); downgrading the migration is optional and independent.
4. Rollout gates (dev-stack kill-9 smoke, post-deploy observation, rollback drill) live in `openspec/changes/agent-stale-run-sweeper-rollout/`.

## Open Questions

- Retention-time GC failures only log today; whether a follow-up recovery job (or thread-first retry ordering) is worth it can be answered from field data — no spec or approach change needed either way.
- Whether a future run-history UI should render `stale_run` distinctly from other failures is a `/agent/runs` slice concern, not this one.
