# Design

## Context

Today the client-visible id is overloaded: the SPA echoes one id per turn and the server decides what it means (live run → resume; live session → continuation with `persist_run_state = False`; otherwise fresh). Continuation turns therefore have no `agent.runs` row at all, which is what broke approval decisions (see proposal.md — Why). The agent already streams the server-minted run id to the client (`run_id` SSE frame → `onRunId` in `chatService.ts`), and langGraph checkpoint threading keys on the run id. `agent.runs` is owned by the agent Alembic schema; the sweeper, retention GC, and GDPR flows already operate on it generically.

## Goals / Non-Goals

Goals:
- Every turn — fresh or continuation — gets its own short-lived `agent.runs` row (heartbeat, one terminal transition, token total) linked to its session.
- Approval decisions resolve against the run row alone for all new turns; the session fallback survives only for pre-existing conversations.
- Wire contract compatible both directions (old SPA + new backend, new SPA + old backend).

Non-Goals:
- Changing the sessions/messages schema or the chat-history UI.
- Removing the legacy session-ownership fallback in the approvals endpoint (kept this change; candidate for a later cleanup once pre-mint conversations age out of approval TTLs).
- Client-maintained run-id history (server-side derivation stays the single source of truth).

## Decisions

**D1 — Server-side minting, not client bookkeeping.** The server derives thread identity from the echoed id and mints the execution id itself, then publishes it on the existing `run_id` frame. Alternative (SPA tracks per-session run-id lists) rejected: client state is unreliable across devices/tabs/cleared storage/mid-stream drops, and the server can already derive everything it needs from durable tables.

**D2 — Detection order unchanged, outcome changed for continuation.** For an echoed id: non-terminal owned `agent.runs` row → resume (`graph.get_state`, unchanged); live owned `chat_sessions` row → continuation, but now `run_id = create_run(user_id, session_id=<echoed id>)` and the full run-state lifecycle applies; neither → fresh session (mints both). A terminal runs row over a live session still means continuation (the finished row is never mutated). LangGraph `thread_id` keys on the per-turn run id, so a continuation turn naturally starts with an empty checkpoint — same observable behavior as today's state-layer bypass, but with real ops metadata.

**D3 — `session_id` column, backfilled once.** Nullable column added by an Alembic revision in the agent schema; `create_run(user_id, session_id=None)`. A single idempotent backfill links historical rows whose id equals a live session id (`UPDATE agent.runs r SET session_id = r.id FROM chat_sessions c WHERE c.id = r.id AND r.session_id IS NULL`) — that is exactly the rows the old first-turn path minted. Nullable + backfilled keeps GDPR export and per-session forensics meaningful without a NOT NULL migration risk.

**D4 — Session id threaded through graph state.** `AgentState` gains `session_id`; chat-history call sites (`create_session`, `touch_session`, `record_message`, title generation) key on it instead of overloading `run_id`. Audit: `executor.py`/`graph.py` touch the run id only for state keying — expected no change, verified by tests.

**D5 — Approvals endpoint: runs-row-first, legacy fallback retained.** For new turns the per-turn row exists and the first lookup hits; the chat-session fallback (hotfix) only serves conversations started before this change. Fail-closed semantics, single 404 sentinel, and first-wins idempotency are untouched.

**D6 — Deploy order: backend first.** New backend + old SPA: safe — the SPA keeps echoing the session id, the server mints per-turn ids, and the SPA already displays whatever `run_id` frame arrives. New SPA + old backend: unsafe in one way — the SPA would echo a per-turn run id the old backend resolves to a terminal row + no session, minting a *fresh session* and silently chopping the thread. The frontend change therefore ships only after the backend is live; this is the change's one ordering constraint (tracking task below; no production gates in this change).

## Risks / Trade-offs

- [Three-state misclassification (resume vs continuation vs fresh)] → adversarial matrix per the agent-slice checklist: disconnect at terminator, cross-user echo, soft-deleted session, terminal-row-over-live-session, mid-stream drop + reattach. Each lands as a test before the entrypoint refactor merges.
- [langGraph checkpoint threads grow ~one per turn instead of one per session] → confirm the retention GC window covers thread deletion at the new volume; add an explicit task; volume is bounded by turns-per-day (small, single-user dev today).
- [`agent.runs` row volume grows from per-conversation to per-turn] → retention already prunes runs by age; re-check the window and add a coverage assertion.
- [One extra INSERT + heartbeat writes per turn] → negligible against multi-second LLM turns; pooled asyncpg; no action beyond existing pool sizing.
- [Parallel frontend work stream editing `chatStore`/`chatService`] → coordinate file ownership before the frontend slice; keep the diff to the resume-path id choice + types.
- [Backfill touches historical rows] → guarded, idempotent UPDATE run manually during rollout verification, not at boot; dev-DB rehearsal first.

## Migration Plan

1. Land backend + migration + tests; agent boots apply the Alembic revision automatically (additive, nullable column — old code ignores it, so rollback of the agent binary alone is safe).
2. Run the backfill manually against the dev DB; verify approvals-on-continuation e2e end-to-end.
3. Land the frontend slice (resume sends the streamed run id) once the backend is deployed.
4. Rollback: revert the agent binary; nullable column is inert; sessions/messages unaffected; approvals keep the legacy fallback in both generations of code.

## Open Questions

None blocking. (GDPR export may naturally become richer — one row per turn — but the erasure/listing requirements are unchanged; no spec delta.)
