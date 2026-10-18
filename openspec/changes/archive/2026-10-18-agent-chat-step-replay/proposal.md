# Proposal

## Why

Leaving a chat session and switching back loses the agent's step trace: the live run rendered `rag` + `llm` steps, the replayed panel renders none. Replay rebuilds steps only from the persisted `tool_calls` array, and native step kinds (`rag`, `llm`) never emit `tool_call` frames — only `tool` / `react` dispatches do. A `[rag, llm]` run therefore persists `tool_calls = NULL` and replays as a bare answer. Observed 2026-10-18 on a 2-step `[rag, llm]` turn: live showed both steps, switching away and back showed zero. The step trace and token usage of the live and replayed views of the same turn must agree.

## What Changes

- `agent.chat_messages` gains a nullable `steps jsonb` column (Alembic migration). It carries the assistant row's step trace: per-step `id`, `kind`, terminal `status`, tool name/args/result, retry count, and failure reason. NULL means "no trace available": user rows, pre-migration rows, and runs that planned no steps.
- The agent's run drain accumulates the trace from the SSE frames it already forwards (`plan`, `step_start`, `tool_call`, `tool_result`, `step_end`; `confirm_request` contributes only the step's tool identity, with no approval-card state) and persists it with the assistant row at finalize, alongside the existing `tool_calls`.
- `GET /agent/sessions/{id}/messages` gains `steps` on every message (`list | null`). `tool_calls` stays on the wire unchanged for compatibility.
- The GDPR export (`GET /agent/me/export`) includes the same `steps` field on exported message rows, so the dump stays a superset of replay.
- The SPA replay mapper prefers the persisted `steps` trace and falls back to deriving steps from `tool_calls` for rows written before this change. Live streaming is untouched.
- The existing `agent-chat-history-store` requirement is corrected while it is being amended: its scenario claims "one assistant row per step + the final answer", but the implementation persists one assistant row per run (carrying the whole trace). The delta rewrites that scenario to match actual behavior.

Non-goals:

- No backfill. Rows without a trace keep `steps = NULL` and fall back to `tool_calls`; historical `[rag, llm]` turns stay stepless.
- No SSE wire change and no new frames.
- No approval-card persistence: a replayed session continues to render no live confirmation card.
- No partial-failure banner replay: a run that diverged still persisted no banner marker, so replay shows the steps and answer without the banner. The parity goal is scoped to the step trace and token usage.
- Token-split persistence is not touched (landed in `fbe85a2`, migration `0007`).

## Capabilities

### New Capabilities

(none)

### Modified Capabilities

- `agent-chat-history-store`: adds the `steps` column and the persistence contract for the assistant row's step trace; corrects the row-per-step scenario drift.
- `agent-chat-sessions-api`: the message-replay response gains `steps`; `tool_calls` and the rest of the envelope are unchanged.
- `agent-gdpr-erasure`: exported message rows gain the same `steps` field, keeping the export a superset of replay.

## Impact

- `backend/agent_py/agent/main.py` — drain accumulates the step trace from frames it already sees, then passes it to `record_message`.
- `backend/agent_py/agent/chat_store.py` — `MessageRow`, `record_message`, `list_messages`, `list_all_messages_for_user`, `_row_to_message`.
- `backend/agent_py/agent/api/chat_sessions.py`, `agent/api/gdpr.py` — message serializers gain `steps`.
- `backend/agent_py/migrations/versions/0008_*.py` — nullable `steps jsonb`; applies on agent boot like `0007`.
- `frontend/src/features/chat/types/chat.ts`, `services/chatService.ts`, `store/chatStore.ts` — replay type, parser, and `toReplayMessage`; legacy `tool_calls` fallback preserved.
- Tests: Python store/API/drain tests, frontend parser/store tests, and an e2e that switches away from a live run and back, asserting both the step rows and the token split survive the replay.
- Wire compatibility: additive nullable field. An old agent with a new SPA hits the fallback; a new agent with an old SPA ignores the unknown field.
- Coordination: the in-flight `agent-per-turn-run-ids` change also edits `main.py`, `chat_store.py`, and `chat_sessions.py`; land order must be agreed before implementation.
- Deploy is a routine agent restart with the auto-migration; no feature flag or env flip, so no rollout gates are foreseen.
