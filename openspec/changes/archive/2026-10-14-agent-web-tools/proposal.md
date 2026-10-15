# Proposal

## Why

The agent can only reach the user's own feed data (MCP registry tools + RAG bridge); it has no way to answer questions about the outside world or read a URL the user supplies. Users asking "what's new in X" or "summarize this link" get a model-prior answer that silently omits the live web. Adding `web_search` (Tavily Search API) and `web_fetch` closes that gap while keeping egress out of the Go API service — they are implemented agent-native, inside the Python orchestrator.

## What Changes

- **New agent-native tools** in `backend/agent_py`: `web_search` (Tavily Search API — keyless by default, optional free key — configurable base URL for test stubs) and `web_fetch` (arbitrary-URL retrieval with an SSRF guard mirroring the Go backend's `checkHostNotPrivate` + per-dial re-check pattern).
- **Second tool source in the dispatcher**: the executor's single dispatch funnel (`_dispatch_tool_with_retries`) gains a local registry alongside the MCP `ToolsClient`. Local tools are discovered by the same `list_tools()` call the react loop already makes, and dispatch local-first with MCP fallback. Local tool failures map onto the existing retry classification (`arg` / `transient` / `non_retryable`) so the retry machinery works unchanged.
- **Planner awareness**: the planner prompt gains a conditional block advertising the web tools when enabled (the planner is catalog-blind today — the block is the minimal, honest fix for agent-native tools).
- **Fetch safety contract** (new, enforced in code + spec): scheme http/https only; port 80/443; resolved-IP deny-list (loopback / RFC1918 / link-local / multicast / unspecified / IPv6 ULA, incl. IPv4-mapped); connection pinned to the validated IP to defeat DNS rebinding; manual redirect following with per-hop re-validation; content-type allowlist; response byte cap; HTML→text extraction (stdlib `html.parser`); output char cap.
- **Untrusted-content framing**: fetched page text and search snippets are wrapped in `<untrusted_web_content>` markers and the tool descriptions state the content is data, never instructions (the repo's Untrusted-inputs rule applied at the tool boundary).
- **Config + gating**: `PYTHON_AGENT_WEB_FETCH_ENABLED` / `PYTHON_AGENT_WEB_SEARCH_ENABLED` (both default **false**; search runs keyless without `PYTHON_AGENT_TAVILY_API_KEY`, which is optional), plus caps/timeouts. Disabled tools are absent from the merged `list_tools()` — react-loop behavior is unchanged when off.
- **Spec delta note**: no rollout sibling is foreseen — the feature is default-off and completable in dev/CI; production enablement (env flip + key provisioning) rides the in-flight, PM-owned `agent-production-rollout`.

Not breaking: wire contract (SSE frames), step kinds, and MCP tool surface are unchanged. Registry tool names are never shadowed — a local tool whose name collides with a registry tool is refused.

## Capabilities

### New Capabilities

- `agent-web-tools`: `web_search` / `web_fetch` tool contracts, the fetch safety guard, search provider configuration, local-dispatch integration (merge + precedence + failure classification), planner catalog exposure, and enable/disable gating.

### Modified Capabilities

- `agent-python-service`: the "Step kinds cover the agent surface" requirement currently pins `tool` steps to MCP `tools/call` dispatch; it changes to "MCP registry tools OR agent-native tools (local-first)", and the "Configurable via environment" requirement gains the web-tool env vars.

## Impact

- **Code**: `backend/agent_py/agent/` — new `web_tools.py` + `net_guard.py` (+ dispatcher), edits to `config.py`, `executor.py`, `planner.py`, `main.py`; tests under `backend/agent_py/tests/`.
- **Frontend/e2e**: new `frontend/tests/e2e/chat-web-tools.spec.ts` replaying pinned SSE fixtures through the app's own parser (deterministic, no live model/network); a live-stack smoke needs no key (search runs keyless) and is documented, not assumed.
- **Docs/config**: `backend/agent_py/README.md`, `backend/agent_py/.env.example`, `docker-compose.yml` agent env passthrough, `FUTURE_AGENT.md`.
- **Dependencies**: no new Python packages (httpx already present; HTML stripping via stdlib).
- **Spec overlap to be aware of**: canonical `agent-tool-registry` (Go) keeps "the registry is the only place tool metadata lives" for MCP-exposed tools — untouched by this change; agent-native tools are never exposed via MCP. Canonical `agent-loop`'s "registry dispatch" phrasing predates the Python service; this change does not amend it (see design.md » Spec overlap).
