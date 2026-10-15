# Proposal

## Why

The chat agent can talk about the user's feeds (RAG) but cannot act on them. The five MCP feed tools (`list_feeds`, `get_feed`, `create_feed`, `update_feed`, `delete_feed`) are live behind `MCP_ENABLED=true`, and the executor already merges MCP `tools/list` into react loops. Two gaps keep the capability inert — and one more makes it unsafe to switch on:

1. **The planner is catalog-blind.** Its prompt advertises only the web tools; nothing tells it `list_feeds` or `create_feed` exist, so "create a feed titled X" plans an apologetic `llm` step. The pre-Python smoke in `agent-production-rollout` 1.1 ("trigger a chat that produces a `list_feeds` plan") cannot pass today.
2. **The MCP wire shape silently loses the argument schema.** LangChain's `convert_to_openai_tool` accepts mcp-go's native tool dict (`{name, description, inputSchema}`) but drops `inputSchema` entirely — verified against langchain-core in `backend/agent_py/.venv`: the converted function carries no `parameters`. Every bound MCP tool reaches the model schemaless, and `_tool_schema_text` (retry re-prompts) reads a `parameters` key that never existed on this shape. Tests used hand-written OpenAI-format fixtures, so both sides were green while the real wire degrades — the repo's wire-contract rule in action.
3. **Mutations must not be autonomous.** An LLM calling `delete_feed`/`update_feed` on the wrong id is the user's data gone (soft-delete, but invisibly). The agent needs an explicit approve/deny step before any mutating MCP call — built on the existing run/heartbeat infrastructure, not beside it.

## What Changes

- **Registry schema normalization (consumer side).** `ToolsClient` outputs are normalized from mcp-go's native shape to the OpenAI function format LangChain binds (`inputSchema` → `parameters`), with `annotations` preserved on a side channel. Fixes the silent schema drop; retry re-prompts gain the real argument schema.
- **Dynamic planner catalog.** The planner prompt gains a "Registry tools available" block built from the live MCP `tools/list` at plan time (name + description per tool), so plans can target whatever the bearer-scoped registry actually serves — the five feed tools today, future MCP tools for free. Block omitted when the registry list fails or is empty (prompt then byte-identical to today). No new flag: `MCP_ENABLED` remains the single deliberate gate, and the catalog can never drift from the served surface.
- **Confirmation before mutating calls.** A tool whose annotations say `readOnlyHint != true` (create/update/delete after the Go-side annotation below) pauses at the shared dispatch funnel (`_dispatch_tool_with_retries` — covers plan `tool` steps and react loops alike) and emits a new SSE `confirm_request` frame carrying tool name, args, and an `approval_id`. The user approves or denies via a new authenticated route; the same stream continues with either the real dispatch, or a model-visible "denied by the user" tool result the model can adapt to. Reads (`list_feeds`, `get_feed`) execute without confirmation. Decisions are brokered through Redis (correct across agent replicas) with a bounded, configurable TTL; the held stream keeps heartbeating so proxies don't cut it.
- **Go MCP annotations.** The five feed tools get MCP tool annotations (`readOnlyHint=true` on list/get; `readOnlyHint=false` + `destructiveHint=true` on create/update/delete) so the confirmation policy is data-driven from the wire — no hardcoded Python name list, and any future mutating MCP tool is gated automatically.
- **New wire surface**: SSE frame kind `confirm_request`; Python `POST /agent/approvals`; Go `POST /api/chat/approvals` (JWT-authed proxy).

Not breaking: existing frame kinds, the single-turn fallback, external MCP clients (annotations are additive), and the agent's behavior when the registry is empty/unreachable (no catalog block, no gate, reads unchanged).

## Capabilities

### New Capabilities

- `agent-mcp-tools`: how the agent consumes the MCP registry — MCP-native schema normalization to the bindable shape, dynamic planner-catalog exposure, and the readOnlyHint-driven confirmation policy for mutating calls (approve / deny / timeout semantics, model-visible denial).

### Modified Capabilities

- `agent-python-service`: new `confirm_request` SSE frame kind, new `/agent/approvals` endpoint under the verified-user-header contract, and the approval-TTL env key.
- `chat-agent-proxy`: new JWT-authed `POST /api/chat/approvals` route forwarding decisions to the Python service.

## Impact

- **Python** `backend/agent_py/agent/`: `tools_client.py` (normalization + annotations), `tool_dispatch.py` (registry-only view for the planner), `planner.py` + `graph.py` (catalog block; planner node gains the dispatcher), `executor.py` (pre-dispatch gate in the shared funnel), new approvals module (Redis decision bus + waits), `main.py` (endpoint + frame), `config.py` (TTL key), `heartbeat/run_metadata` interplay during the wait.
- **Go** `backend/internal/`: `mcp/service.go` (annotations), `chat/` (new route + handler + tests), `cmd/api/main.go` wiring.
- **Frontend** `frontend/src/features/chat/`: parse `confirm_request`, approve/deny card (tool + args), call the new endpoint, render the continued stream; component tests + Playwright e2e (create-with-approve lands the feed; deny leaves feeds untouched; `list_feeds` runs unconfirmed).
- **Docs/config**: `backend/agent_py/README.md`, `backend/agent_py/.env.example`, `docker-compose.yml` passthrough, `FUTURE_AGENT.md` (ticks the agent-loop-era feed-tool items).
- **Cross-runtime contract**: the mcp-go `tools/list` shape (native schema + annotations) is the pinned Go↔Python interface — consumer tests must round-trip a captured Go wire fixture, per the repo's wire-contract rule.
- **Rollout**: no sibling rollout change foreseen — the surface is default-off (`MCP_ENABLED`, `AGENT_ENABLED`) and completable in dev/CI; production enablement (MCP_ENABLED flip in prod + approval-path smoke against a deployed stack) rides the in-flight, PM-owned `openspec/changes/agent-production-rollout/`, mirroring the agent-web-tools precedent.
