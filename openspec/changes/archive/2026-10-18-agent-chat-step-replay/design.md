# Design

## Context

See `proposal.md` — Why. The relevant current state:

- `backend/agent_py/agent/main.py`'s run drain already sees every SSE frame it forwards (`plan`, `step_start`, `tool_call`, `tool_result`, `confirm_request`, `step_end`) and accumulates `tool_call` payloads verbatim into `assistant_tool_calls` for the finalize write.
- `chat_store.record_message` persists `tool_calls`, `tokens`, and the usage split onto one assistant row per run. `list_messages` / `list_all_messages_for_user` read it back; `api/chat_sessions.py` and `api/gdpr.py` serialize the row.
- `frontend/src/features/chat/store/chatStore.ts` `toReplayMessage` rebuilds `ChatStep[]` from `tool_calls` only; the live fold in `send` is the reference behavior the replay must reproduce.
- Migration `0007` (usage split) established the nullable-column-on-boot pattern; the agent applies Alembic migrations at startup.

## Goals / Non-Goals

**Goals:**

- Persist the same step trace the live panel rendered, for every completed run that planned steps.
- Make replay (session switch, panel reopen, reload) and live streaming render the same steps and token usage.
- Keep the wire and storage changes additive: old rows, old clients, and old agents keep working through one fallback path.

**Non-Goals:**

- Reconstructing traces for rows persisted before the column exists.
- Persisting in-flight approval cards or a paused run's live state.
- Normalizing steps into their own table; the trace is read and written whole with its message.
- Changing the SSE frames themselves.

## Decisions

### D1 — A dedicated nullable `steps jsonb` column

Add `agent.chat_messages.steps jsonb NULL` (migration `0008`). Do not overload `tool_calls`: that field is a wire-faithful record of real tool dispatches (`tool` / `react` kinds only) and is already consumed by replay and export; adding synthetic `rag` / `llm` entries would change its meaning for every consumer. A separate column makes "no trace" representable as NULL and keeps migration/rollback trivial.

Alternatives considered: a normalized `chat_message_steps` table (write amplification, ordering, and no query need — rejected); deriving steps at replay time from `tool_calls` only (cannot recover native step kinds — rejected).

### D2 — Fold in the drain, not from graph state

Accumulate the trace in `main.py`'s drain loop, from the exact frames the client received. This guarantees replay parity with what the live UI rendered and reuses the one place that already collects `assistant_tool_calls`; the graph state's `step_outputs` lacks statuses, retry counts, and failure reasons.

The fold is a small pure helper (`StepTrace` in a focused module, e.g. `agent/step_trace.py`) so it can be unit-tested without the FastAPI app:

- `plan` → seed one entry per planned step, in plan order: `{id, kind, status: "running", tool_name, tool_args}`.
- `step_start` → upsert; set `kind`, `status: "running"`.
- `tool_call` → upsert; set `tool_name`, `tool_args` (latest wins, matching the UI).
- `tool_result` → upsert; set `tool_result` (`result` on ok, `error` on failure), `retry_count`.
- `confirm_request` → upsert; set `tool_name`, `tool_args`; record no approval-card state.
- `step_end` → upsert; set `status` (`ok` / `failed`) and `reason` when present.
- Entries keep insertion order; folding by `step_id` means a resumed run that re-emits frames is idempotent.

`to_list()` returns `None` when empty so the column stays NULL for no-plan runs.

### D3 — Persisted entry shape (snake_case, optional fields)

```json
{
  "id": "step_1",
  "kind": "rag",
  "status": "ok",
  "tool_name": "web_search",
  "tool_args": {"query": "..."},
  "tool_result": "...",
  "retry_count": 1,
  "reason": "..."
}
```

`id`, `kind`, `status` are always present; the rest are omitted when the run never supplied them. Field names follow the replay wire's existing snake_case convention (`tool_calls`, `prompt_tokens`). The store serializes with `json.dumps` exactly as it does for `tool_calls`; the frames are already JSON-safe (Pydantic `model_dump`).

### D4 — Replay prefers `steps`, falls back to `tool_calls`

`toReplayMessage` maps a non-empty `steps` array into `ChatStep[]`; when `steps` is NULL or empty it keeps the current `tool_calls`-derived mapping. This preserves legacy rows (which can still show real tool dispatches) and keeps the new path total for new rows. `SessionMessage` gains a parsed `steps: SessionStep[] | null`; the parser drops malformed entries rather than failing the whole row, matching `toSessionMessage`'s defensive posture.

### D5 — Export and replay share the row serializer

Both `api/chat_sessions.py` and `api/gdpr.py` add `"steps": message.steps`. `MessageRow` decodes the JSONB once in `_row_to_message`, so neither API re-parses.

### D6 — Storage writes stay best-effort

The finalize write keeps its current contract: `ChatStoreUnavailable` is warn-logged and never fails the stream. The fold runs in memory during the drain; `record_message` owns the trace's fallback: it degrades to `steps=None` (warn-logged) when Python serialization fails (`allow_nan=False` rejects NaN/Infinity), and retries the insert once without the trace when PostgreSQL rejects the jsonb (NUL escapes, and any other SQLSTATE-class-22 rejection). The assistant row and `end_session` therefore still land — a bad trace never suppresses the message. The row is written only when the run reached `done`, so a partial-failure run still leaves no assistant row (existing behavior).

## Risks / Trade-offs

- **Resumed runs**: a resume may re-emit frames for steps executed in a prior phase. Folding by step id makes this idempotent; the trace reflects the latest fields the wire carried. Verified by a fold unit test with duplicate frames.
- **Trace size**: react steps collapse to one entry per step (latest call/result), so iteration count does not grow the entries; a single `tool_result` is stored verbatim, so one large result (e.g. a web fetch capped at 20k chars) can size the column. No cap is added in this change; a bounded preview is a candidate follow-up if row size becomes a problem.
- **Sensitive data**: `tool_args` / `tool_result` can contain user data. They are already stored in `tool_calls` and `content`; the new column inherits the same retention and GDPR erasure/export treatment because it rides on the message row.
- **Agent/SPA version skew**: an old SPA ignores the new field; a new SPA against an old agent sees NULL and falls back. No error path.
- **Deploy**: the migration is additive and nullable; downgrade drops the column and loses only traces. No feature flag, so no rollout gates are foreseen.

## Migration Plan

1. Ship migration `0008` + agent changes; the agent applies it on boot (same as `0007`).
2. Rows written from then on carry `steps` when the run planned steps; pre-existing rows stay NULL and replay via the fallback.
3. Rollback: downgrade drops the column (traces lost, messages intact) and the SPA fallback keeps replay working against old rows.
