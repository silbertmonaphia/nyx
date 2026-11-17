# Design

## Context

See `proposal.md` — Why. Relevant current state:

- `backend/agent_py/agent/graph.py` wires `START → planner → executor → END`; `executor` is one node that loops over `plan.steps` (`executor.py`).
- LangGraph (1.2.14) checkpoints once per superstep. `build_graph` compiles with an `AsyncPostgresSaver` and no `durability` argument, which defaults to `"async"` — the checkpoint write is scheduled but not awaited before the next superstep starts.
- `run_agent_stream` drives `graph.astream(stream_mode="values")` and flushes each chunk's new events through the `RunSink`; on resume it passes `None` as input so the graph continues from the checkpoint.
- The executor already skips steps whose id is in `step_outputs`; today that only helps when the async checkpoint writer happens to serialize the live, in-place-mutated channel values after a step completed.
- Wire contracts that must not change: `agent-loop` (frame order and step ids), `agent-partial-failure` (the note precedes the final answer's `delta` frames whenever they have not already been streamed; a later step's failure follows already-streamed deltas — see this change's delta spec), `agent-python-service` (resume note and terminal frames), `agent-tool-retry` (dispatch/retry behavior). Side-effect dedupe for the step that must re-run is the sibling `mcp-tool-call-idempotency` change.

## Goals / Non-Goals

**Goals:**

- One plan step per graph superstep, with a checkpoint committed at every step boundary before the next step starts.
- A crash resumes at the first unfinished step; at most the in-flight step re-runs.
- Checkpoints written by the current two-node graph remain resumable after the deploy (and the reverse, for rollback).
- Every existing frame, handler, and terminal path behaves exactly as today — including the partial-failure note's position before the final answer's deltas.

**Non-Goals:**

- De-duplicating side effects of the in-flight step that re-runs (covered by `mcp-tool-call-idempotency`).
- Checkpointing inside a ReAct step's iteration loop (the whole step re-runs, capped as today).
- Run leases, drain, horizontal scaling, or frame-replay changes.
- Changing any step handler's business logic.

## Decisions

### D1: A self-loop on the existing `executor` node, not a subgraph

The graph becomes `START → planner → executor → (executor | END)`, where `executor` executes **at most one plan step per invocation**. The conditional edge returns to `executor` while steps remain and the run has not terminated.

Why:

- Flat checkpoint thread: no nested subgraph namespaces, no second compiled graph, no extra checkpointer wiring. The checkpoint schema and retention path are unchanged.
- The node keeps the name `executor`, so checkpoints written by today's graph (whose pending node is `executor`) still resolve after the deploy.
- `run_agent_stream` keeps its positional per-chunk event slicing; each chunk now covers one step.

Alternatives considered:

- **Checkpointed executor subgraph** with per-step nodes inside. Encapsulates the loop, but adds nested checkpoint namespaces and with `stream_mode="values"` the parent yields a single chunk for the whole subgraph, coarsening the non-sink streaming path and complicating resume diagnostics.
- **Manually calling `aupdate_state` per step.** Writes a checkpoint while the node is running on the same thread — unsupported and racy (possible fork), and duplicates what the framework already does at node boundaries.

### D2: An explicit `step_cursor` channel

Add `step_cursor: int` to `AgentState` (default 0 in `new_state`). The node increments it exactly once per invocation — including a step that failed and a step skipped by the defensive resume guard. Routing uses `cursor >= len(plan.steps)`.

Why not derive progress from `step_outputs`: a failed step writes no output, so derivation would re-run it forever. A cursor also distinguishes "failed" from "not yet run", which `_collect_failed_steps` already relies on via `step_end` frames.

Old checkpoints have no `step_cursor` channel; reads use `.get("step_cursor", 0)`.

### D3: Routing

After `executor`, route to `END` when `terminal_status != "ok"` (budget/plan terminal paths already emitted their frames) or when the cursor has reached the end of the plan; otherwise route to `executor`. The planner → executor edge stays unconditional so the existing defensive `plan is None → terminal failed/no_plan` branch still runs exactly as today.

### D4: End-of-plan finalization stays inside the last step's invocation

`_maybe_emit_partial_failure` runs after the cursor reaches the plan length, *before* the node returns — not in a separate finalize node. The final answer's `delta` events are appended by the last step inside the same node invocation, so the note is inserted before them in the event list and both flush together at the node boundary, preserving `agent-partial-failure`'s ordering contract. The call is idempotent, so it is safe when it also runs on the defensive-skip path or after a budget termination already emitted the note.

### D5: `durability="sync"` whenever a checkpointer is present

The installed LangGraph (>= 1.2) exposes `durability` on `astream` / `ainvoke`, not on `compile()`, so `run_agent_stream` passes `durability="sync"` whenever the graph carries a checkpointer. LangGraph awaits the checkpoint write at the end of each superstep before starting the next one, so a completed step's output is durable before the next step begins — the guarantee the spec states.

Trade-off: one extra Postgres round-trip per step. Steps are LLM/tool/RAG calls that take hundreds of milliseconds to seconds, so a few milliseconds of write latency is negligible; plan size is capped at 10. The alternative (default `"async"`) leaves a window where the last completed step can also re-run, which weakens the contract to "at most two steps re-run" and keeps the outcome timing-dependent.

### D6: Keep the defensive `step_outputs` skip

For new-format runs the cursor makes the skip redundant. It stays as the compatibility path for checkpoints written by the current two-node graph: pending node `executor`, cursor absent (0), partial `step_outputs` possibly present from the old async-writer race. In that case the skip prevents re-running steps whose outputs were captured, and the cursor advances past them. It also keeps rollback safe: the old executor ignores `step_cursor` and continues to skip completed steps.

### D7: Step handlers are lifted, not rewritten

The existing `_run_rag` / `_run_kb` / `_run_tool` / `_run_llm` / `_run_react` and the dispatch/retry/approval machinery move behind the one-step node unchanged. Their tests and the wire frames they emit stay byte-identical. The `RunSink` identity-based delivery already tolerates mid-list insertion and multiple flushes, so it needs no change.

### D8: Frame timing improves; frame content does not

With one superstep per step, `run_agent_stream` flushes after each step instead of buffering the whole executor into one chunk. Users see `step_start` / `tool_call` / `step_end` as steps progress, even without an approval pause forcing a mid-node flush. This is a timing improvement, not a wire-contract change; tests that assumed a single executor chunk must be updated.

### D9: Idempotency sequencing

`mcp-tool-call-idempotency` derives its key from `(run_id, step_id, tool, canonical_args)`. All four are checkpointed, so replaying the in-flight step derives the same key. This change therefore needs no idempotency code; it relies on that change landing first so a replayed mutating call is deduped.

## Risks / Trade-offs

- **Sync checkpoint latency per step** → only when a checkpointer is configured; bounded by plan cap (10) and dominated by the step's own I/O. If profiling later shows a problem, `durability` can become a setting without changing the graph.
- **More checkpoint rows per run** (one per step rather than two) → plans are capped at 10 steps; the existing retention cron deletes whole threads, so no new retention path is needed.
- **Old in-flight checkpoints mid-step** → the old writer may have captured partial frames but no `step_outputs` for the in-flight step; resume re-runs that step and replays its frames, exactly as today. The defensive skip handles any captured outputs.
- **A mid-step approval-gate flush can precede the final-answer deltas** in a ReAct step that emits text, pauses for a mutating tool, then finishes and inserts the partial-failure note. This ordering edge exists today (mid-node flush) and is unchanged.
- **A failure in a step after the answering step now emits the note after already-streamed deltas** — unavoidable once each step flushes on completion (buffering deltas until the plan ends would undo D8). The `agent-partial-failure` delta spec states the relaxed ordering; the SPA renders the banner by event kind, and the note still precedes `done` / `terminator`. The identity-based delivery path is tested for exactly-once delivery with and without a sink.
- **Tests that assert one executor chunk** will fail until updated → the task list updates them together with the graph change.
- **Live e2e requires killing and restarting the side-stack agent** → `scripts/e2e-side-stack.sh` already owns the agent pidfile, so the spec can stop/start it; serial `--workers=1` per the live-stack convention.

## Migration Plan

- Pure code deploy to the Python agent; no migrations, config, or Go/frontend changes.
- Forward compatibility: old checkpoints resume because the node name is unchanged, `step_cursor` defaults to 0, and the defensive skip handles captured step outputs.
- Rollback: reverting the code restores the two-node graph; new-format checkpoints' pending node is still `executor` and the extra `step_cursor` channel is ignored by the old state shape.
- No env flips or staged rollout gates; no sibling rollout change.
