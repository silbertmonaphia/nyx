# Proposal

## Why

Mutating tool calls can execute twice. The executor mints a fresh `call_id` per dispatch attempt (`backend/agent_py/agent/executor.py`), so a transient retry after a response loss re-sends an already-committed write. A run resumed from a checkpoint re-executes the in-flight step, replaying side effects that already landed. `create_feed` can duplicate rows; a replayed `delete_feed` returns 404 after the first call succeeded. Neither retry nor resume has a dedupe mechanism today.

This must land before per-step checkpoints and agent horizontal scaling make run resume routine — otherwise every pod crash becomes a data-integrity event.

## What Changes

- **Agent key generation**: once per intent, mint a stable `idempotency_key` = `uuid5(run_id | step_id | tool | canonical_args)`. It survives transient retries, re-prompts with unchanged arguments, and checkpoint resume. `call_id` stays per-attempt for SSE frames. Generate the key before the approval gate so a crash-after-approval retry dedupes.
- **Agent injection**: the executor derives the key at dispatch time and overrides any model-supplied value; the model never generates or sees it. Attach it only when the tool's served schema declares the property — no hardcoded tool names.
- **MCP surface**: `create_feed`, `update_feed`, `delete_feed` gain an **optional** `idempotency_key` argument (length/charset validated) with description guidance for external clients: derive one key per logical operation, reuse it for identical retries, use a new key for a new operation, omit to opt out. The agent's model-bound schema excludes the property; the dispatcher reads the raw served schema to decide injection.
- **Go write path**: owner-scoped idempotency record written in the **same Postgres transaction** as the mutation. On conflict, return the stored original response, so a repeated delete returns the first success rather than 404. Absent key = no dedupe.
- **Retention**: bounded cleanup of idempotency records.
- **Tests**: Python key-stability unit tests, Go tool/DB tests, an agent→MCP wire-contract test, and a chat-UI e2e asserting a retried/resumed mutating call produces one feed row.
- **Docs**: MCP tool descriptions, agent README / `FUTURE_AGENT.md`.

Explicit non-goals (separate changes): per-step LangGraph nodes/checkpoints, run lease + drain, Redis frame mirror, run supervisor, BYOK credential persistence.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `agent-tool-retry`: transient retries and re-prompts with unchanged arguments reuse one idempotency key; corrected arguments form a new intent and a new key.
- `agent-mcp-tools`: served schemas advertise and document the optional property; the model-bound and re-prompt schemas exclude it while the dispatcher reads the raw schema to inject the runtime-derived key.
- `agent-tool-registry`: mutating dispatches dedupe per owner and key inside the same transaction as the write, replaying the stored response on duplicates.

## Impact

- `backend/agent_py/agent/executor.py`, `tool_dispatch.py`, `tools_client.py`
- `backend/internal/tool/feeds.go` (argument structs + dispatch wrapper), `backend/internal/mcp/service.go` (tool descriptions)
- `backend/migrations/` (new idempotency table + retention index), `backend/queries/` + sqlc if queries are introduced
- `backend/internal/feed/` or a small idempotency store package
- Tests: `backend/agent_py/tests/`, `backend/internal/tool/`, `backend/internal/mcp/`, `frontend/tests/e2e/`
- Docs: `backend/agent_py/README.md`, `FUTURE_AGENT.md`

No rollout gates: the change is code + migration + tests, with no production env flip. No sibling rollout change.
