# Proposal

## Why

LLM credentials (chat model, embedding model, Tavily search) are server-wide `.env` values today — one key set shared by every user, managed by the operator. 需求.md asks for per-user BYOK: each user configures their own model endpoints and keys from the SPA, stored per user in Postgres, with the Go backend and Python agent consuming the user's values at request time. Without it, users cannot point the agent at their own providers (DeepSeek, a local vLLM, any OpenAI-compatible endpoint), and the operator's keys are a shared, unattributed resource.

## What Changes

- New `user_llm_settings` table (migration 000017, public schema, Go-owned): 11 fields per user — main LLM (base URL, API key, model), embedding (base URL, API key, model), Tavily (base URL, API key) required; retry-model LLM (base URL, API key, model) optional.
- New huma endpoints `GET/PUT/DELETE /api/me/llm-config` (Bearer-auth, user id from the JWT claim only): GET returns masked keys (tail-4 only), PUT upserts, DELETE clears. API keys are **write-only on the wire** and **AES-GCM encrypted at rest** (`v1:`-prefixed ciphertext, `USER_LLM_ENCRYPTION_KEY`, validated at boot when `LLM_ENABLED=true`). The endpoints exist exactly when the LLM feature exists (`LLM_ENABLED=true`); with the feature off the chat route is absent too, so the SPA hides the gear button in the same condition.
- **Strict BYOK** (operator decision — no `.env` fallback): a user with no config gets HTTP 409 on `POST /api/chat` — the repo's standard error envelope (`error` message, numeric `code` = 409, machine-readable marker `llm_not_configured` in `details`) — empty retrieval from the RAG bridge, and no feed embedding indexing — feed CRUD itself is unaffected (index skip stays best-effort). Clearing config re-gates immediately.
- Go consumes per-user values everywhere LLM work happens: chat (agent payload + legacy single-turn router incl. optional fallback), RAG query embedding, feed indexing — via a bounded per-user client cache over the unchanged `llm.Provider` seam (openai implementation for all user configs, so private/localhost endpoints work). **Two clients per user** (chat slot + separate embedding slot) — `openai.Client.Embed` posts to the *chat* client's base URL, so the user's embedding endpoint needs its own client.
- Both user base URLs are OpenAI-compatible endpoints and **must carry the `/v1` path prefix** (the openai SDK appends `/chat/completions` and `/embeddings`); the modal states the convention and its placeholders show it, because the server-level `.env` convention is asymmetric (the vllm impl takes a bare host).
- Go forwards `llm_config` + `tavily_config` to the Python agent inside the `/agent/run` payload (server-constructed, never client-supplied, never logged). The agent threads the override through every LLM client construction site and the web-search tool; absent override keeps `Settings` fallback (backward compatible for direct/test use). The override travels **outside the checkpointed graph state** — the production agent checkpoints every node transition into the `langgraph` schema, so the plaintext key must never enter the state.
- Planner structured-output binding becomes a per-run decision: a user base-URL host containing `deepseek` runs the legacy free-text planner path; otherwise the global flag applies. The planner now honors the user's configured model (behavior change: was `PYTHON_AGENT_LLM_PLANNER_MODEL`).
- The router's liveness probe path is normalized for `/v1`-suffixed base URLs (`{base}/v1/models` would 404 on an openai-convention base and silently fail every request over to the user's fallback). Pre-existing bug, user-visible once per-user routers exist.
- SPA: gear button next to the AI Agent button opens an LLM-config modal (4 sections, examples as placeholders, masked saved keys, clear-config with confirm). Opening AI Agent with no saved config force-opens the modal blocking until configured; a chat 409 also opens it.
- Server-level `LLM_*` env vars stay constructed and validated (smallest diff) but stop serving user-facing paths; documented as such.

Not changing: the `llm.Provider` interface, SSE frame set, RAG bridge wire contract (unconfigured retrieval stays a 200-empty envelope), feed CRUD semantics, MCP, confirmation/approval flows, embeddings schema (`feed_embeddings` stays `vector(1024)` — user embedding models must emit 1024 dims; a mismatch degrades to the empty envelope, documented).

## Capabilities

### New Capabilities

- `user-llm-config`: per-user BYOK LLM configuration — settings table + AES-GCM at rest, masked-key CRUD endpoints scoped to the JWT identity, update validation (required fields, URL scheme, placeholder-key blocklist, empty-key-keeps-existing, fallback all-or-nothing), strict-BYOK gating of chat/RAG/indexing, and server-side forwarding of config to the Python agent per run without persisting the key in the agent runtime.

### Modified Capabilities

- `chat-agent-proxy`: `/agent/run` payload carries server-constructed `llm_config`/`tavily_config`; chat requests from unconfigured users fail with 409 before any SSE byte; single-turn parity now scoped to configured users; the per-user fallback router probes the provider's models endpoint correctly.
- `feed-embed-indexing`: indexing resolves the feed owner's per-user embedder; an unconfigured owner skips indexing (still best-effort, still healable — and a later configuration MUST be picked up by the next backfill, which pins the backfill-guard ordering).
- `agent-rag-bridge`: retrieval for a user without config returns the same 200-empty envelope as no matches (never an error, never a cross-user leak); a dimension-mismatched embedder degrades to the same envelope.
- `agent-planner-structured`: the structured binding is selected per run from the user provider's capability heuristic (deepseek host → legacy path).
- `agent-web-tools`: `web_search` reads provider base URL and key from the per-run override when present, else configuration.

## Impact

- `backend/migrations/000017_create_user_llm_settings.{up,down}.sql` (new, reversible, no data backfill); `backend/sqlc.yaml` + `backend/queries/llmsettings.sql` + generated `internal/llmsettings/db`.
- `backend/internal/llmsettings/` (new domain: model/repository/service/resolver/huma_handler); `backend/internal/platform/crypto/` (new AES-GCM helper); `backend/internal/platform/config/config.go` (`USER_LLM_ENCRYPTION_KEY`, exported placeholder-key check); `backend/cmd/api/main.go` wiring.
- `backend/internal/chat/` (409 gate, `AgentRunPayload`, per-user provider in legacy path); `backend/internal/rag/` (embedder resolver, backfill-guard ordering, dimension guard); `backend/internal/llm/router.go` (probe path normalization); `backend/internal/feed/` (unchanged code — the index-failure path is already warn-only).
- `backend/agent_py/agent/`: `types.py`/`state.py` (override models), new `llm_effective.py`, 7 `ChatOpenAI` sites, `web_tools.py`, `main.py` run handler.
- `frontend/src/features/chat/` (types/service/modal/hook, chatStore 409 gate), `frontend/src/App.tsx` (gear button + blocking gate), `frontend/src/i18n/{en,zh}.ts`.
- `frontend/tests/e2e/`: shared register flow seeds a config (existing specs keep passing); new agent-free `chat-llm-config.spec.ts`.
- New env: `USER_LLM_ENCRYPTION_KEY` — `backend/.env.example` only. The compose backend service reads `env_file: backend/.env` and deliberately carries no `environment:` block, so the key is not added to `docker-compose.yml` or the root `.env.example` (both would override or duplicate `backend/.env`). OpenAPI regenerated (`make openapi` + frontend `openapi:gen`).
- Rollout gates: none — dev/CI-completable end to end (the strict-BYOK user break is deployment-local and takes effect when the operator deploys, not via a staged prod flip), so per the rollout-split classification test there is **no sibling rollout change**.
- Known accepted break: on deploy, every existing user's chat 409s until they configure (explicit operator decision); the SPA surfaces it by auto-opening the config modal. The encryption key must be identical across replicas and stable across restarts — changing it makes every stored row undecryptable (fail-closed), which the deploy note records.
