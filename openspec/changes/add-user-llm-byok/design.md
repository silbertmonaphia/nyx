# Design

## Context

LLM config is process-global today: viper `.env` → clients constructed once in `cmd/api/main.go` → injected as `llm.Provider` into `chat.Service` / `rag.Embedder`. Both runtimes own disjoint Postgres schemas (Go: `public`; Python: `langgraph`+`agent`, structurally barred from `public`), and the agent receives identity as the verified `X-Nyx-User-Id` header. The feed indexer and `rag.BackfillOnce` are already user-scoped. See proposal.md for motivation; behavior contract in specs/.

## Goals / Non-Goals

**Goals:**

- One per-user config row, CRUD via the SPA gear modal, keys encrypted at rest and masked on read.
- Strict BYOK gating at every LLM consumption point, with feed CRUD untouched.
- Python stays schema-isolated: config reaches it per run in the request payload; it never reads `public`.
- Zero change to the `llm.Provider` interface, the SSE frame set, and the RAG bridge wire contract.

**Non-Goals:**

- Save-time connectivity/dimension probes (1024-dim embedding constraint is documented, not enforced).
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
The `llm/vllm` client's per-dial guard refuses private addresses, which would outlaw the primary use case (localhost vLLM, `host.docker.internal`, LAN Ollama/LM Studio). The openai SDK transport has no per-dial check, so user configs accept http/https anywhere; validation is parse + scheme + no-userinfo only. Self-hosted deployment accepted the SSRF surface explicitly. Server-level `LLM_*` behavior (incl. `LLM_ALLOW_PRIVATE_URL`) is unchanged.

**D4 — AES-256-GCM with a versioned token format; key = `sha256(USER_LLM_ENCRYPTION_KEY)`; `v1:<base64(nonce||ct)>`.**
Random 12-byte nonce per encryption (`crypto/rand`); decrypt fails closed on unknown prefix or auth failure (wrong key after rotation invalidates rows loudly, not silently). `MinKeyBytes = 32` enforced in both `config.Load()` (when `LLM_ENABLED=true` — the key is load-bearing exactly when the LLM feature exists; demanding it on `LLM_ENABLED=false` deployments would break every no-LLM boot/test) and `NewCipher` (mirrors the JWT_SECRET double-check).

**D5 — Keys are write-only on the wire; GET returns `api_key_masked` (tail-4).**
On PUT, an empty key field means "keep the stored ciphertext" (create requires it present); plaintext exists only in-memory in the service/resolver. This is the standard BYOK-settings shape (OpenRouter-style) and keeps key material out of browser memory, logs, and any client store.

**D6 — Strict BYOK with a 409 before the first SSE byte.**
`stream()` resolves the user's config after request validation and before dispatch: `ErrNotFound` → `api.WriteError(409, "LLM configuration required", "llm_not_configured")` — a plain JSON envelope in the window already reserved for pre-stream errors (chat has no other 409, so the SPA can key on status alone; a wire-contract test pins this). RAG bridge keeps its 200-empty envelope for unconfigured users (frozen Python-side contract). Feed indexing skips with a warning (existing best-effort semantics absorb the sentinel). `.env` fallback is rejected per operator decision.

**D7 — Resolver = bounded client cache keyed by config hash, not per-request construction.**
`sha256(base_url|key|model)` per slot; hash compare on each resolve (a PUT invalidates within one request); map+mutex, cap 64 with arbitrary eviction — user counts are small, and clients hold `*http.Client` state so unbounded construction leaks. Clients build via the clone-not-mutate pattern over the base config; fallback present → `llm.NewRouter` with two slots, mirroring `mountChatRoute`.

**D8 — Resolve BEFORE `BackfillOnce`'s `LoadOrStore`.**
The `sync.Once` guard is stored before the work runs today; a user who configures LLM later would be poisoned (marked done on a run that did nothing) until process restart. Ordering fix: resolver miss (`ErrNotFound`) returns without marking. Pinned by a named test.

**D9 — Planner structured-output selection is a per-run heuristic in Python; planner honors the user model.**
`structured_supported = settings.planner_structured_enabled and "deepseek" not in <override host>`. URL heuristics are imperfect; the planner's bounded self-check retry remains the safety net for providers whose structured output misbehaves. With an override present, the planner uses the user's model (was `settings.planner_model`) — otherwise a user's configured model silently applies to only half their run.

**D10 — Tavily override threads through the existing per-request tool seam.**
`ToolDispatcher` is already constructed per request; only the local-tool registry is shared at startup. When a run carries `tavily_config`, `build_local_tools` builds a per-run registry; otherwise the shared one is untouched.

## Risks / Trade-offs

- **Deploy-time user break (accepted)**: every existing user 409s on chat until configured. Discoverability: the SPA auto-opens the modal on panel-open and on the 409; native clients get the standard error envelope (documented).
- **Embedding dimension drift (documented, not enforced)**: `feed_embeddings.embedding` is `vector(1024)`; a user configuring a 1536-dim model degrades safely (index upserts warn-and-skip, retrieval returns empty) but silently. Modal helper text + README note; a save-time probe is a follow-up.
- **Key transit Go→Python in plaintext** rides the internal hop with the forwarded bearer token (same trust domain); never logged; Langfuse masking already scrubs credential-shaped strings.
- **Encryption key loss = stored keys unrecoverable** (same blast radius as dropping the table); versioned format enables future dual-key rotation.
- **`openai`-impl choice drops per-dial SSRF guarding for user URLs** — explicit operator decision (self-hosted private endpoints are the point); residual risk documented.
- **Planner-model behavior change** is observable (planner quality now depends on the user's model); called out in FUTURE_AGENT.md and the commit body.
- **No comments on new lines** (repo rule) — rationale lives here, in tests, and in commit messages.

## Migration Plan

1. Migration 000017 (CREATE TABLE / DROP TABLE, reversible, empty at start) applies on backend boot — deploy order: backend first (new route + 409 gate), frontend second (modal + gate), Python agent any time (tolerates absent `llm_config` forever; only new payload fields are new).
2. `USER_LLM_ENCRYPTION_KEY` must be set before the first `LLM_ENABLED=true` boot; missing/short key refuses startup (loud, same as JWT_SECRET).
3. Rollback: revert binaries, keep the table (harmless, preserves user configs). `migrate down 1` destroys saved configs — operator-visible in the down migration.

## Open Questions

None — the four product decisions (masked keys, private URLs allowed, strict BYOK, AES-GCM at rest) were confirmed by the operator during planning.
