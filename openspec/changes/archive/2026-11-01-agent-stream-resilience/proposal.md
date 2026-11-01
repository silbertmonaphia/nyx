# Proposal

## Why

A chat run's SSE connection is its only liveness signal, and the wire has three compounding fragilities:

1. **A cut is invisible.** The SPA parser ends its generator normally when the body dies mid-run — no `done`, no `error`, no `[DONE]` — so the store treats a broken stream exactly like a clean end: `isStreaming: false`, no error, a half-built answer stranded in the UI. The user never learns the stream died, so the (already working) `run_id` resume path is never triggered.
2. **The keepalive that exists is not contractual.** The agent already emits `: keep-alive` comment frames after 15 s of drain idleness (landed with agent-feed-tools for the approval pause, hardcoded in `_drain_run_queue`) — but the cadence is not configurable, so operators on paths with tighter proxy timeouts have no dial, and the SPA has no watchdog that treats its absence as a dead-stream signal.
3. **Disconnect kills the run.** Today a mid-run client disconnect cancels the Python run generator; recovery is a checkpoint re-run on the next manual re-POST — re-executing the in-flight step (double upstream token spend) and regenerating an answer that diverges from the partial text the user already saw.

The industry pattern for LLM streaming is that the SSE connection is a *view* onto a durable server-side run. The pieces already exist in this deployment — `agent.runs` heartbeat, LangGraph checkpointer, `step_trace` event persistence, shielded terminal writes — they are just wired to die with the connection.

## What Changes

- **Keepalive made contractual.** The existing drain-side idle keep-alive (`: keep-alive` comment frames, already shipping at a hardcoded 15 s) gains `PYTHON_AGENT_KEEPALIVE_SECONDS` (default 15, `0` disables). The Go proxy already forwards comment frames verbatim and the SPA parser already skips them — the new consumer-side piece is a watchdog that treats keepalive absence as a dead-stream signal.
- **A cut becomes a detectable state.** `streamMessage` yields an explicit stream-incomplete signal when the body ends without the app having delivered `done` / `error` / `[DONE]`. A broken stream can no longer masquerade as a clean end.
- **Automatic reconnect ladder in the SPA.** On an incomplete stream: re-attach to the live run → fall back to transcript replay → fall back to a resume re-POST echoing `run_id`. Exponential backoff with jitter, bounded attempts, cancelled by user actions. This replaces "the user notices and types again" as the only recovery path.
- **The run outlives its connection.** With `PYTHON_AGENT_STREAM_DETACH_ENABLED=true` (the default — the flag is a kill switch), a mid-run client disconnect no longer cancels the run: run execution lives in a supervisor task that outlives the response, keeps its heartbeat (so the sweeper's semantics are untouched), stays bounded by `PYTHON_AGENT_DETACHED_RUN_CAP_SECONDS` (default 600 s, aligned with the Go proxy's 10 m cap), and its terminal writes land as usual. Setting the env to `false` restores today's disconnect-cancels-run semantics exactly.
- **A re-attach endpoint.** `GET /agent/runs/{run_id}/stream` (service-token + user-header contract, same as `/agent/run`): a live, owned run streams `note{kind:"reattached"}`, then its persisted step events replayed for context, then live frames, then the terminal trio; a pending approval is re-emitted as `confirm_request`. A non-live, unknown, or non-owned run is one indistinguishable typed 409; persistence disabled is a typed 501. Go exposes it as `GET /api/chat/runs/{run_id}/stream` behind the existing auth group and the per-user stream limiter, proxied byte-for-byte with the same stream cap and double-terminate guard as the POST path.
- **Cancel becomes an explicit call.** With the connection no longer a kill signal, deliberate stop needs its own channel: `POST /agent/runs/{run_id}/cancel` (Go: `POST /api/chat/runs/{run_id}/cancel`) cancels the supervised run — `failed{code:"user_cancelled"}`, no assistant row for the partial answer, subscribers see the typed error frame — and a cancelled run never resurrects (terminal state makes the next echo a session continuation). The SPA's Cancel button fires the call first, then aborts locally regardless of outcome; the run-in-flight marker gains a stop control for backgrounded runs; `user_cancelled` renders as stopped-by-user, not an error toast.
- **Splice semantics on reconnect.** A re-attach appends continuation into the *same* partial assistant row; a resume re-POST *replaces* the partial row's content with the regenerated answer. Neither duplicates rows or concatenates two different answers.

Documented limitation: deltas emitted between the cut and the re-attach are not replayed — a mid-answer reconnect can show a gap of a few sentences. Frame-level replay (Last-Event-ID / stream buffering) is explicitly deferred.

Not changing: the legacy single-turn path, the frame envelope (`event:`/`data:`/`\n\n`), the resume decision tree's keying (live run → session → fresh), the sweeper's reap semantics, the approval flow's wire shape, byte-for-byte forwarding in the Go proxy.

## Capabilities

### New Capabilities

- `chat-stream-resilience`: the SPA's broken-stream contract — distinguishing an incomplete stream end from a clean end, the reconnect ladder (re-attach → transcript replay → resume re-POST), backoff and cancellation, read-idle watchdog, splice semantics for the partial assistant row, and the cancel presentation (explicit endpoint call, background-run stop control, stopped-by-user state).

### Modified Capabilities

- `agent-python-service`: mid-run client disconnect no longer cancels the run when detach is enabled (terminal persistence requirement gains the detached-run mode); keepalive cadence made configurable; the re-attach endpoint; the cancel endpoint; the three new env keys with startup validation.
- `chat-agent-proxy`: the re-attach route is proxied to the agent under the existing auth group — new requirement, additive; the existing cap / double-terminate / byte-for-byte requirements are reused, not changed.

## Impact

- `backend/agent_py/agent/`: `config.py` (three keys + validation), `main.py` (keepalive env key, supervisor decoupling of run lifetime from the stream generator, re-attach handler, cancel handler), `approvals.py` (comment-frame queue items already exist), `step_trace.py` (replay read path), `run_metadata.py` (detached-cap failure path).
- `backend/internal/chat/`: new `runs_proxy.go` (re-attach + cancel routes, limiter, cap, tests); `RegisterChatRoute` gains `GET /runs/{run_id}/stream` and `POST /runs/{run_id}/cancel`.
- `frontend/src/features/chat/`: `services/chatService.ts` (incomplete-stream signal, re-attach generator), `store/chatStore.ts` (ladder, backoff, watchdog, splice, cancel-then-abort flow, stop control on the run-in-flight marker), types, i18n locale strings, vitest suites.
- `frontend/tests/e2e/`: `chat-stream-resume` live spec (cut → reconnect → completion; cut → background completion → transcript replay).
- `backend/agent_py` env docs, `backend/.env.example` / root `.env.example` where the agent keys are documented, `README.md` + `README.zh` agent sections, `FUTURE_AGENT.md` / `FUTURE_BACKEND.md` / `FUTURE_FRONTEND.md` ticks.
- No migrations, no new dependencies, no sqlc changes.
- Rollout gates foreseen (green-window wait, staging disconnect drill, observation of detached-run metrics against the default-on build): tracked in `openspec/changes/agent-stream-resilience-rollout/`. There is no production env-flip task — the default is the rollout, and the kill switch (`PYTHON_AGENT_STREAM_DETACH_ENABLED=false`) is the rollback.
