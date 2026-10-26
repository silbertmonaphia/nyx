# Proposal

## Why

Session-continuation turns (every turn after the first in a conversation) run with `persist_run_state = False`: they mint no `agent.runs` row, skip heartbeat, and never write a terminal state. The approval decision endpoint (`POST /agent/approvals`) verifies ownership solely against `agent.runs`, so every `confirm_request` raised inside a session-first conversation (SPA "New chat" → `POST /agent/sessions` never produces a runs row) 404s on decision submission. Observed in production 2026-10-17: user approves `create_feed`, the decision is rejected as `404: run not found`, the UI surfaces a server error, and the run waits out the approval TTL and closes as denied. A hotfix (session-table ownership fallback in the decision endpoint) shipped the same day; this change removes the underlying id overloading instead of piling more compat rules on it.

The id overloading has a second cost: continuation turns have no operational record at all — no liveness heartbeat for the stale-run sweeper, no per-turn token accounting in `agent.runs`, no terminal state for debugging — for exactly the turns real conversations are made of.

## What Changes

- `POST /agent/run` mints a **fresh, per-turn `run_id`** (with a full `agent.runs` row: heartbeat, terminal state, token total) for session-continuation turns, instead of bypassing the run-state layer. The client-echoed id is reinterpreted as the **session id** (thread identity); the canonical execution id is delivered to the client via the existing `run_id` SSE frame, which the SPA already consumes (`onRunId`).
- `agent.runs` gains a nullable `session_id` column linking each per-turn row to its conversation thread (Alembic migration in the agent schema).
- The crash-recovery resume rule is unchanged and keeps priority: a non-terminal `agent.runs` row for the echoed id still means "reattach to that execution".
- The approval decision endpoint resolves ownership via the run row first; the session-table fallback shipped in the hotfix is demoted to a legacy-compat path (and becomes dead code for new turns).
- Wire contract stays backward compatible: the request field keeps its name and semantics ("thread hint"), old clients echoing the session id keep working, and the SPA requires only a small change to send the streamed run id on resume-after-disconnect.

## Capabilities

### New Capabilities

(none)

### Modified Capabilities

- `agent-python-service`: "Session continuation on echoed session ids" — continuation turns now mint a per-turn `agent.runs` row and surface a fresh run id on the wire; the no-row/no-mutation rule is replaced by per-turn rows linked to the session. "Approval endpoint accepts decisions under the verified-user-header contract" — ownership is proven by the per-turn run row; the session-table lookup becomes a legacy fallback.
- `agent-run-store-py`: `RunMetadata.create_run` accepts the owning session id; `agent.runs` carries a nullable `session_id` column; per-turn rows follow the same lifecycle rules (heartbeat, single terminal transition, sweeper reaping) as fresh-run rows.

## Impact

- `backend/agent_py/agent/main.py` (entrypoint three-state detection + terminal persistence), `state.py` (new `session_id` state key), `approvals_api.py`, `run_metadata.py` (+ Alembic migration), `chat_store.py` call sites that key on the run id as session id, `graph.py`/`executor.py` only where the run id keys state (audit: no behavior change expected).
- Frontend: `frontend/src/features/chat/services/chatService.ts` (already routes `event: run_id` → `onRunId`) and `chatStore` (send the streamed run id for resume-after-disconnect; keep sending the session id as the thread hint). Note: another work stream has in-flight edits in this area — coordinate before touching shared files.
- Consumers verified unchanged: `agent-run-sweeper` (reaps per-turn rows by heartbeat — unchanged criteria), `agent-budget-guard-py` (Redis-based — unaffected), `agent-gdpr-erasure` (`delete_all_for_user` cascades by user — unchanged; export may gain richer per-turn history), `agent-chat-titles` / `agent-chat-history-store` (keyed by session id — call sites re-pointed, semantics unchanged).
- `agent.runs` row volume grows from one-per-conversation-start to one-per-turn; retention (`retention.py`) already prunes runs by age — re-check the retention window against the new volume.
- No production env flips in this change. Rollout gates, if any surface during implementation, go in `openspec/changes/agent-per-turn-run-ids-rollout/`.
