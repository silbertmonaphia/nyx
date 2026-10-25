# Tasks

## 1. Migration + sqlc

- [ ] 1.1 Create `backend/migrations/000017_create_user_llm_settings.{up,down}.sql`: `user_llm_settings` (user_id INTEGER PK → users(id) ON DELETE CASCADE; 11 config columns with `_enc` key columns; fallback columns `DEFAULT ''`; timestamps); down drops the table. Verify: up/down apply cleanly against the integration-test container.
- [ ] 1.2 Add `backend/queries/llm_settings.sql` (`GetUserLLMSettings :one`, `UpsertUserLLMSettings :one` with `ON CONFLICT (user_id) DO UPDATE ... RETURNING *`, `DeleteUserLLMSettings :execrows`) + a fourth `sqlc.yaml` domain block (schema: users + 000017 up files; out `internal/llmsettings/db`; column renames to `LLMBaseURL` style). Run `make sqlc`. Verify: `make sqlc-diff` clean, generated code committed.

## 2. Crypto + config

- [ ] 2.1 Create `backend/internal/platform/crypto/gcm.go`: `MinKeyBytes=32`, `NewCipher` (key = sha256(secret), length-checked), `Encrypt` → `v1:<base64(nonce||ct)>` with a fresh 12-byte `crypto/rand` nonce, `Decrypt` failing closed on unknown prefix/auth failure. Verify: roundtrip, wrong-key, unknown-version, short-key, format tests.
- [ ] 2.2 Add `UserLLMEncryptionKey` to `config.go` (field + BindEnv); `Load()` refuses empty/short when `LLM_ENABLED=true`. Export the placeholder-key blocklist (`CheckNotPlaceholderKey`). Add the var to `backend/.env.example`, root `.env.example`, `docker-compose.yml` backend env. Verify: config tests — required-when-enabled, optional-when-disabled, short-key refusal.

## 3. `internal/llmsettings` domain

- [ ] 3.1 `model.go` + `repository.go`: domain struct + `ErrNotFound`/`ErrInvalidConfig`; narrow `Querier` (3 methods) + `Repository` + `sqlRepository`; `pgx.ErrNoRows → ErrNotFound`; `init()` registers `ErrInvalidConfig` → 400 (ErrNotFound stays internal). Verify: pgxmock repo tests — get-not-found, upsert roundtrip, delete rows-affected, user scoping.
- [ ] 3.2 `service.go`: `MaskKey` (tail-4), `validateUserURL` (parse, http/https, host required, userinfo rejected — no DNS/private checks), `ValidateUpdate` (8 required on create, blank-key-keeps-existing on update, fallback all-or-nothing, placeholder-key rejection). Verify: table-driven service tests for each rule.
- [ ] 3.3 `resolver.go`: `Resolve(ctx, userID) (*UserStack, error)`; bounded client cache keyed by `sha256(url|key|model)` (map+mutex, cap 64); clients built clone-not-mutate over the base config with the openai implementation; fallback present → `llm.NewRouter` two slots. Verify: resolver tests — builds, cache hit, rebuild after update, not-found passthrough, cap eviction.
- [ ] 3.4 `huma_handler.go`: `GET/PUT/DELETE /api/me/llm-config` (HumaAuth + BearerAuth, user id from `reqctx` only); GET unconfigured → `{"configured":false}`; PUT → upsert + masked response; DELETE → idempotent 204; OpenAPI descriptions mark keys write-only. Register in `cmd/api/main.go`; run `make openapi` + frontend `npm run openapi:gen`. Verify: handler tests — unconfigured GET, masked GET, blank-key update, invalid URL 400, idempotent 204, unauthenticated 401, cross-user isolation.

## 4. Go strict-BYOK wiring

- [ ] 4.1 `chat/model.go` + `agent_proxy.go`: `AgentLLMConfig`/`AgentTavilyConfig`/`AgentRunPayload` (ChatRequest untouched); `Run` takes the payload; payload fields never logged. Verify: proxy test capturing the `/agent/run` body asserts `llm_config`/`tavily_config` present, `fallback` absent, and records the fixture for the Python-side consumer test.
- [ ] 4.2 `chat/handler.go`: resolve after validation, before dispatch; `ErrNotFound` → 409 JSON `llm_not_configured` (not event-stream); resolve failure → ClassifyAndLog + 500; agent mode builds the payload from the resolved stack; legacy mode routes through a new `Service.ChatWithProvider` (Chat delegates with `s.provider`; nil resolver → byte-identical behavior). Verify: handler tests — 409 in both modes with non-SSE content type, legacy per-user provider, existing byte-parity tests untouched.
- [ ] 4.3 `rag/service.go` + `indexer.go`: `SetEmbedderResolver`; `Retrieve` resolves per user, unconfigured → internal sentinel → bridge 200-empty envelope; `BackfillOnce` resolves BEFORE `LoadOrStore` (no marking on unconfigured runs); `Indexer.Index` unconfigured → warn-only sentinel. Verify: rag tests — per-user retrieve, unconfigured sentinel, backfill no-poison (unconfigured → configure → second call runs), index skip; feed test — sentinel never fails the write.
- [ ] 4.4 `cmd/api/main.go`: construct cipher (refuse bad key when LLM enabled) → repo → service → resolver; register huma ops; inject resolver into chat mount, RAG bridge, `SetEmbedderResolver` on embedder/indexer. Verify: `SKIP_CONTAINERS=true go test ./...` green; `golangci-lint` clean.

## 5. Python agent

- [ ] 5.1 `types.py` + `state.py`: `LLMOverride`/`TavilyOverride` models; `ChatRequest` gains optional `llm_config`/`tavily_config`; state carries them; `new_state` threads them. Verify: consumer-side wire test parsing the Go payload fixture through `ChatRequest.model_validate`; absent fields default to None.
- [ ] 5.2 New `agent/llm_effective.py`: `resolve_llm(state, settings, *, planner)` (override wins; planner uses the override model; executor precedence `state.model > override.model > settings.llm_model`) + `structured_supported(base_url, settings)` (global flag AND no `deepseek` in host). Verify: unit tests for both, including no-override fallback to Settings.
- [ ] 5.3 Thread the effective config through all 7 `ChatOpenAI` sites: `planner.py` structured + legacy, `executor.py` ×3, `main.py` title refinement, `prefs.py` extraction (thread override params where needed). Verify: patched-client tests asserting base_url/api_key/model at every site, with and without override.
- [ ] 5.4 `web_tools.py` + `main.py`: `search_web`/`build_local_tools` take an optional Tavily override; the run handler builds a per-run local registry only when an override is present. Verify: web-tools tests — override endpoint/key used, keyless tier preserved when no key anywhere, shared registry untouched without override.
- [ ] 5.5 Run `backend/agent_py` pytest once bare (no `TEST_ASYNC_PG_DSN`) and once with it. Verify: both exit 0.

## 6. Frontend

- [ ] 6.1 `features/chat/types/llmConfig.ts` + `services/llmConfigService.ts` (get/update/remove over the shared axios instance). Verify: vitest — payload matches the Go contract fixture; null sections coerced.
- [ ] 6.2 `components/LlmConfigModal.tsx` + `hooks/useLlmConfig.ts`: RHF+zod (8 required, 3 optional, fallback superRefine), example placeholders in every empty input, masked saved-key placeholder (blank = keep), per-section guidance copy (what the group drives, where to obtain credentials incl. provider console URLs + local-vLLM option, feature-impact note; embedding section states the 1024-dim requirement with an example model), destructive clear with confirm, toasts, no key material in any persisted store. Verify: vitest — validation blocking, blank-key submit, masked placeholder, guidance copy present per section, clear confirmation.
- [ ] 6.3 `App.tsx` gear button (i18n `nav.llmConfig` in en+zh) + gate: AI Agent open with no config → blocking modal (auth-gate pattern) until saved; save closes and opens the panel; gear opens the dismissible modal; `chatStore` 409 handling opens the gate. Verify: vitest — gate opens on unconfigured, 409 opens the gate.

## 7. E2E + docs

- [ ] 7.1 `tests/e2e/helpers/llm-config.ts`: `seedLlmConfig` + shared `registerAndOpenChat` (register → seed → open panel); replace per-spec copies; update `auth.spec.ts` register-asserts-chat-form cases. Verify: affected specs green locally.
- [ ] 7.2 New agent-free `tests/e2e/chat-llm-config.spec.ts`: fresh user sees the blocking modal; fill+save → panel opens; gear reopen shows masked keys/blank key fields/prefilled rest; clear+confirm → re-gated; routed 409 on `/api/chat` opens the gate. Verify: `npx playwright test tests/e2e/chat-llm-config.spec.ts` green; then full `npm run test:e2e`.
- [ ] 7.3 Dev-stack e2e per the closed-loop gate: `make up` → backend with `USER_LLM_ENCRYPTION_KEY` + `LLM_ENABLED=true` via `make dev` → wait healthy, confirm 000017 applied → run the new spec + full agent-free set. `@live` set only against the side stack, `--workers=1`. Verify: suites green.
- [ ] 7.4 Docs: FUTURE.md §6, FUTURE_BACKEND.md §8/§9 + config section (`USER_LLM_ENCRYPTION_KEY`), FUTURE_FRONTEND.md §11, FUTURE_AGENT.md (payload extension + planner-model change); README + zh README per-feature section via identifier grep; mark server `LLM_*` as no longer serving user paths. Verify: identifier grep finds all updated surfaces.
- [ ] 7.5 Drift + coverage gates: `make sqlc-diff`, `make openapi-diff`, `make coverage-check` (new packages ≥70%), `openspec validate --strict` on the change. Verify: all exit 0.
