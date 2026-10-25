# Design

## Context

LLM config is process-global today: viper `.env` → clients constructed once in `cmd/api/main.go` → injected as `llm.Provider` into `chat.Service` / `rag.Embedder`. Both runtimes own disjoint Postgres schemas (Go: `public`; Python: `langgraph`+`agent`, structurally barred from `public`), and the agent receives identity as the verified `X-Nyx-User-Id` header. The feed indexer and `rag.BackfillOnce` are already user-scoped. See proposal.md for motivation; behavior contract in specs/.

Two structural facts shape the wiring:

- `mountChatRoute` returns before the chat route is registered when `LLM_ENABLED=false` — so a deployment with the LLM feature off serves no `/api/chat` at all, and `AGENT_ENABLED=true` without `LLM_ENABLED=true` does not produce an agent-backed chat route either. The RAG facade and the bridge are built in the same function, so they share the gate.
- The openai client binds the chat base URL, the API key and the chat model at construction, and `Client.Embed` posts to that *same* base URL. The server-level wiring therefore builds a second client when a separate embedding endpoint is configured (`mountRAGFacade`). The vllm client instead appends `/v1/...` to a bare host, which is why the `.env` base-URL convention differs per provider.

## Goals / Non-Goals

**Goals:**

- One per-user config row, CRUD via the SPA gear modal, keys encrypted at rest and masked on read.
- Strict BYOK gating at every LLM consumption point, with feed CRUD untouched.
- The user's *embedding* endpoint is honoured — it gets its own client, not the chat endpoint.
- Python stays schema-isolated: config reaches it per run in the request payload; it never reads `public`, and the plaintext key never reaches its checkpoint tables or traces.
- Zero change to the `llm.Provider` interface, the SSE frame set, and the RAG bridge wire contract.

**Non-Goals:**

- Save-time connectivity/dimension probes (the 1024-dim embedding constraint is documented, not enforced at save time).
- Encryption-key rotation machinery (`v1:` prefix reserves the seam).
- Per-user fallback semantics in the agent (fallback stays Go legacy-router only).
- Server `.env` cleanup on user paths (validation/construction stay; follow-up).
- Admin view of user configs.

## Decisions

**D1 — Config table in Go's `public` schema; Python consumes via payload, never via DB.**
Go owns identity and CRUD; forwarding the resolved values in `/agent/run` preserves the agent's structural `public`-exclusion invariant and needs no new internal endpoint. *Alternative*: agent reads its own `agent.*` config table — rejected, it would put Go rows in the Python-owned schema chain or split the config across two owners.

**D2 — `llm_config`/`tavily_config` ride a server-side `AgentRunPayload`, not `chat.ChatRequest`.**
`ChatRequest` is the SPA-facing decode target; adding config fields there invites a future caller path to forward client-supplied config (user A injecting user B's provider). The payload is assembled only from the DB row for the JWT-verified user. Plaintext keys ride the same private-network hop as the forwarded `Authorization` header (existing trust model) and are never logged. Fallback is NOT forwarded (Python has no fallback semantics; it serves the Go legacy router only).

**D3 — Per-user clients use the openai implementation for every user, and user URLs are not private-IP-blocked.**
The `llm/vllm` client's per-dial guard refuses private addresses, which would outlaw the primary use case (localhost vLLM, `host.docker.internal`, LAN Ollama/LM Studio). The openai SDK transport has no per-dial check, so user configs accept http/https anywhere; validation is parse + scheme + no-userinfo only. Self-hosted deployment accepted the SSRF surface explicitly. Server-level `LLM_*` behavior (incl. `LLM_ALLOW_PRIVATE_URL`) is unchanged. Consequence: every user base URL follows the openai convention and must end in `/v1` (the SDK appends `/chat/completions` and `/embeddings`), which the modal states and its placeholders show. A loopback base URL (`localhost`, `127.0.0.0/8`, `::1`) needs no API key — local vLLM/Ollama servers accept any Authorization header; the stored key column stays empty and the resolver substitutes its non-credential client sentinel at construction, so no placeholder secret is persisted or returned.

**D4 — AES-256-GCM with a versioned token format; key = `sha256(USER_LLM_ENCRYPTION_KEY)`; `v1:<base64(nonce||ct)>`.**
Random 12-byte nonce per encryption (`crypto/rand`); decrypt fails closed on unknown prefix or auth failure (wrong key after rotation invalidates rows loudly, not silently). `MinKeyBytes = 32` enforced in both `config.Load()` (when `LLM_ENABLED=true` — the key is load-bearing exactly when the LLM feature exists; demanding it on `LLM_ENABLED=false` deployments would break every no-LLM boot/test) and `NewCipher` (mirrors the JWT_SECRET double-check).

**D5 — Keys are write-only on the wire; GET returns `api_key_masked` (tail-4).**
On PUT, an empty key field means "keep the stored ciphertext" (create requires it present); plaintext exists only in-memory in the service/resolver. This is the standard BYOK-settings shape (OpenRouter-style) and keeps key material out of browser memory, logs, and any client store.

**D17 — Every group is optional; the main LLM gates only the AI Agent.**
A configuration may contain any subset of the groups (but not all empty). Chat requires the main LLM: a row without it returns the same 409 `llm_not_configured` envelope as no row, and the SPA gates the agent panel on `llm != null` rather than on row existence. Embedding and Tavily stay independent: embedding-only users still index and retrieve feeds; feed CRUD and the rest of the app never depend on any group.

**D6 — Strict BYOK with a 409 before the first SSE byte.**
`stream()` resolves the user's config after request validation and before dispatch: `ErrNotFound` → `api.WriteError(w, r, http.StatusConflict, "LLM configuration required", "llm_not_configured")`. The response is the repo's standard error envelope — `{"error": <message>, "code": <int HTTP status>, "request_id": ..., "details": ...}` — so the machine-readable marker rides in `details`, **not** in `code` (`ErrorResponse.Code` is an int; the change cannot introduce a string `code` without touching every error surface). A plain JSON envelope in the window already reserved for pre-stream errors (chat has no other 409, so the SPA can key on status alone; a wire-contract test pins this). RAG bridge keeps its 200-empty envelope for unconfigured users (frozen Python-side contract). Feed indexing skips with a warning (existing best-effort semantics absorb the sentinel). `.env` fallback is rejected per operator decision.

**D7 — Resolver = bounded client cache keyed by config hash, not per-request construction.**
`sha256(base_url|key|model)` per slot; hash compare on each resolve (a PUT invalidates within one request); map+mutex, cap 64 with arbitrary eviction — user counts are small, and clients hold `*http.Client` state so unbounded construction leaks. Clients build via the clone-not-mutate pattern over the base config; fallback present → `llm.NewRouter` with two slots, mirroring `mountChatRoute` in `cmd/api/main.go`.

**Two clients per user, not one.** `openai.Client.Embed` posts to the chat client's base URL, so the embedding slot needs its own client — the same shape `mountRAGFacade` already builds for `LLM_EMBEDDING_BASE_URL`. `UserStack` therefore carries the chat provider (a router when a fallback is configured), the embedding provider, and the embedding model. A resolver that reuses one client for both slots would silently point the user's embedding traffic at their chat endpoint — the user's embedding base URL would be accepted, stored, and ignored.

**D8 — Resolve BEFORE `BackfillOnce`'s `LoadOrStore`.**
The `sync.Once` guard is stored before the work runs today; a user who configures LLM later would be poisoned (marked done on a run that did nothing) until process restart. Ordering fix: resolver miss (`ErrNotFound`) returns without marking. Pinned by a named test.

**D9 — Planner structured-output selection is a per-run heuristic in Python; planner honors the user model.**
`structured_supported = settings.planner_structured_enabled and "deepseek" not in <override host>`. URL heuristics are imperfect; the planner's bounded self-check retry remains the safety net for providers whose structured output misbehaves. With an override present, the planner uses the user's model (was `settings.planner_model`) — otherwise a user's configured model silently applies to only half their run.

**D10 — Tavily override threads through the existing per-request tool seam.**
`ToolDispatcher` is already constructed per request; only the local-tool registry is shared at startup. When a run carries `tavily_config`, `build_local_tools` builds a per-run registry; otherwise the shared one is untouched.

**D15 — Embedding and Tavily are optional; their absence disables only the dependent capability.**
The embedding group and the Tavily group are optional; only the main LLM is required. An absent embedding group resolves to no embedder: RAG retrieval returns the empty envelope, feed indexing is skipped (feed CRUD unaffected), and `BackfillOnce` is not marked done so a later configuration heals. A per-user run without a Tavily override builds a per-run local registry WITHOUT `web_search` and never falls back to deployment-configured Tavily (strict BYOK); the planner catalogue derives from that registry, so the agent does not plan searches it cannot run. Both optional sections are collapsible in the modal (closed by default, auto-expanded when configured) and each shows a red warning naming its concrete impact. The embedding model is locked once stored: a different model is rejected by the API and the modal disables the input with a red explanation (switch models by clearing the embedding group first). Removing the embedding group or deleting the config deletes the user's stored vectors, and the modal says so explicitly (section warning while configured, plus the clear-confirmation copy); changing only the base URL or API key with the same model keeps them (the vector space is identified by the model) and resets the per-user backfill guard, so the next retrieval re-embeds with the new model; rotating only the API key keeps the vectors. This closes the silent-mixed-vector-space failure where old and new models produce incomparable embeddings. The retry-model (fallback) group stays in the API and the Go legacy router but is not exposed in the modal yet: the agent has no fallback semantics, so surfacing it would imply behaviour it cannot deliver; it returns when the agent gains one.

**D11 — The plaintext key never enters the checkpointed graph state.**
The production agent checkpoints every node transition through `AsyncPostgresSaver`, which writes the state into the `langgraph` schema verbatim. Anything carried in `AgentState` therefore lands in Postgres in plaintext — including an API key. The override travels *outside* the state: a request-scoped holder consulted at client-construction time, with the state carrying only non-secret fields (base URL, model) plus an opaque handle. The same rule covers traces: the Langfuse/OTel scrubber only matches Bearer tokens, JWTs and `sk-`/`*-lf-` shapes, so a Tavily key or a self-hosted key would survive masking — the guarantee is "never put it in the state or a span", not "the scrubber will catch it".

**D12 — The router's liveness probe path is normalized for `/v1`-suffixed base URLs.**
`llm.Router.probe` builds `{base}/v1/models`, which is correct for the vllm impl's base convention (a bare host) and wrong for the openai impl's (the base must end in `/v1`). With the openai impl chosen for every user config, an unfixed probe would 404 on every request and the router would silently fail over to the user's fallback — the user pays for the fallback provider and never reaches their configured model. The probe normalizes: a base ending in `/v1` is probed at `{base}/models`, otherwise at `{base}/v1/models`. Pre-existing bug, user-visible once per-user routers exist; pinned by a unit test.

**D13 — The config endpoints exist exactly when the LLM feature exists.**
`/api/me/llm-config` is registered with the same `LLM_ENABLED=true` gate as the chat route, and the SPA hides the gear button in the same condition. `USER_LLM_ENCRYPTION_KEY` is required exactly when the endpoints are mounted, which is why the boot check keys off `LLM_ENABLED=true`. Registering the endpoints unconditionally would leave them without a cipher on a no-LLM deployment (an unexplained 500 behind a visible button).

**D16 — The embedding width is validated on save when the endpoint is reachable.**
A save-time probe calls the user's embedding endpoint with the configured model and one short input. A reachable endpoint returning a width different from `EMBEDDING_DIMENSIONS` is a 400 (`embedding_model`); an unreachable endpoint logs a warning and the save proceeds (a temporarily-down provider must not block configuration). The modal states the 1024-dim requirement in red directly under the model input, and the model locks after the first save.

**D14 — A dimension mismatch degrades to the empty envelope, not to an error.**
`feed_embeddings.embedding` is `vector(1024)`; a user whose embedding model emits a different width makes the retriever's `embedding <=> $1` comparison raise a pgvector dimension error, which the bridge's safe-error funnel would surface as a 500 — not the "degrades safely" behaviour the risk register claims. The embedder therefore checks the returned vector width against `EMBEDDING_DIMENSIONS` and reports the same internal "no embedder" sentinel as the unconfigured case → the 200-empty envelope. The indexer's upsert keeps its warn-and-skip behaviour.

## Risks / Trade-offs

- **Deploy-time user break (accepted)**: every existing user 409s on chat until configured. Discoverability: the SPA auto-opens the modal on panel-open and on the 409; native clients get the standard error envelope (documented).
- **Embedding dimension drift (documented, not enforced)**: `feed_embeddings.embedding` is `vector(1024)`; a user configuring a 1536-dim model degrades safely (index upserts warn-and-skip, retrieval returns the empty envelope via D14) but silently. Modal helper text + README note; a save-time probe is a follow-up.
- **Key transit Go→Python in plaintext** rides the internal hop with the forwarded bearer token (same trust domain); never logged, never checkpointed (D11), never attached to a span. The Langfuse scrubber does **not** cover arbitrary user key shapes — only Bearer tokens, JWTs and `sk-`/`*-lf-` patterns are matched — so the never-log guarantee is enforced by construction (the key is not passed to any logging or tracing call), not delegated to the scrubber.
- **Encryption key loss = stored keys unrecoverable** (same blast radius as dropping the table); versioned format enables future dual-key rotation. The key must be identical across replicas and stable across restarts — a per-deploy random key makes every stored row undecryptable (decrypt fails closed) and every user 500s until they re-enter their keys.
- **`openai`-impl choice drops per-dial SSRF guarding for user URLs** — explicit operator decision (self-hosted private endpoints are the point); residual risk documented. The same surface extends to the search tool: a user's Tavily base URL is dialled by the agent, while `web_fetch` still refuses private targets. The asymmetry is intentional and recorded here.
- **Planner-model behavior change** is observable (planner quality now depends on the user's model); called out in FUTURE_AGENT.md and the commit body.
- **No comments on new lines** (repo rule) — rationale lives here, in tests, and in commit messages.

## Migration Plan

1. Migration 000017 (CREATE TABLE / DROP TABLE, reversible, empty at start) applies on backend boot — deploy order: backend first (new route + 409 gate), frontend second (modal + gate), Python agent any time (tolerates absent `llm_config` forever; only new payload fields are new).
2. `USER_LLM_ENCRYPTION_KEY` must be set before the first `LLM_ENABLED=true` boot; missing/short key refuses startup (loud, same as JWT_SECRET). It must be the same value on every replica and across restarts — the deploy note records this.
3. Rollback: revert binaries, keep the table (harmless, preserves user configs). `migrate down 1` destroys saved configs — operator-visible in the down migration.

## Open Questions

None — the four product decisions (masked keys, private URLs allowed, strict BYOK, AES-GCM at rest) were confirmed by the operator during planning.

Follow-ups (not in this change): a save-time probe that a configured model exists on its endpoint and reports its embedding width; rotation support for the encryption key; search providers beyond Tavily (the `web_search` tool is Tavily-shaped, so another provider needs its own request shape, not just a different base URL).
