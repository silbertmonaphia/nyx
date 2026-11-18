# Tasks

## 1. Settings schema and deployment bounds

- [ ] 1.1 Add migration `000022` adding `rerank_base_url`, `rerank_api_key_enc`, `rerank_model` (`TEXT NOT NULL DEFAULT ''`) with a symmetric down migration; verify the backend boots the migration and `cd backend && make sqlc && make sqlc-diff` is clean.
- [ ] 1.2 Extend `Settings`/`EncryptedSettings`, `backend/queries/llmsettings.sql`, and the repository with the rerank fields (encrypt on write, decrypt on read, empty-means-empty); verify `go test ./internal/llmsettings/` covers round-trip encryption, masking, and an all-empty row.
- [ ] 1.3 Add `RERANK_CANDIDATES` (default 20), `RERANK_TIMEOUT` (default 2s), and `RERANK_MAX_DOC_CHARS` (default 4000) to `internal/platform/config` with positive/bounded startup validation and `.env.example` entries; verify config tests reject zero/negative/oversized values naming each variable.

## 2. Reranker client and retrieval stage

- [ ] 2.1 Implement the `rag.Reranker` interface and the HTTP client for `POST {base}/rerank` (`model`, `query`, `documents`, `top_n`; optional bearer key; bounded timeout): verify unit tests against an `httptest` server, including a captured vLLM response fixture checked against the vLLM protocol schema, non-2xx/timeout/malformed-body errors, and that no key appears in any log or error.
- [ ] 2.2 Implement the ordering/merge function (score descending, dense rank ascending tiebreak, ignore out-of-range/duplicate indices and echoed text, unscored candidates keep dense order at the tail, no candidate invented); verify unit tests for every case in the `rag-rerank` spec.
- [ ] 2.3 Integrate the stage into `rag.Service`: `candidateK = max(RERANK_CANDIDATES, requestedK)`, rerank each corpus independently before `fuseCorpora`, dense fallback on any reranker error, per-configuration cooldown after 3 consecutive failures with a post-cooldown probe, and a span/metric for success/fallback/cooldown; verify unit tests for `feeds`, `kb`, and `both` ordering, fallback, cooldown and recovery.
- [ ] 2.4 Add the KB rerank-text helper that reproduces the embedded chunk prefix (`rel_path › heading / path\n\n` + body) and UTF-8-safe truncation at `RERANK_MAX_DOC_CHARS`; verify unit tests cover the prefix shape and boundary truncation.
- [ ] 2.5 Use `rag.ChunkText` as the feed rerank text and confirm disabled/no-group retrieval output is byte-identical to the pre-change dense order; verify the existing `rag` suite plus a dedicated no-group ordering test.
- [ ] 2.6 Wire the per-user resolver: `llmsettings.Resolver` builds the rerank client from the user's rerank group (SSRF transport honoring `BYOK_ALLOW_PRIVATE_URL`, slot-keyed cache), `rag.Service.SetRerankerResolver` is wired in `cmd/api/main.go`, and a user without the group resolves to `nil`; verify resolver tests cover per-slot endpoints, isolation, and cache behavior.

## 3. Settings API and connectivity probe

- [ ] 3.1 Extend GET/PUT/DELETE `/api/me/llm-config` with the `rerank` section: masked key on GET, all-or-nothing group validation on PUT, loopback-keyless and remote-requires-key rules, placeholder rejection, URL validation, rerank-only saves, and removal restoring dense order; verify handler/service tests and a regenerated OpenAPI document (`make openapi-diff` clean).
- [ ] 3.2 Add the rerank slot to `POST /api/me/llm-config/validate` (one short query + one short document, same path as runtime), classify `ok`, `auth_failed`, `rate_limited`, `invalid_model`, `invalid_config`, `unreachable`, `timeout`, `slow_response`, skip empty slots, and never return key material; verify validate tests for each class plus the no-save guarantee.
- [ ] 3.3 Confirm the rerank group is not forwarded in the agent run payload; verify the existing agent-proxy payload tests and add an assertion that no rerank fields or key appear.

## 4. Frontend configuration surface

- [ ] 4.1 Regenerate the OpenAPI TypeScript schema/types and update `LlmConfigModal` fixtures; verify `cd frontend && npm run openapi:check` and typecheck pass.
- [ ] 4.2 Add the collapsible Rerank section to `LlmConfigModal` with URL/model/key inputs, the Jina/Cohere-compatible `POST /rerank` guidance, the red dense-ordering impact warning, example placeholders, and its own test-connection control with per-status inline states; verify component tests cover render, save, mask, probe states, and en/zh i18n key parity.
- [ ] 4.3 Verify the full config flow still loads, edits, clears, and gates correctly for existing groups when the rerank group is absent; verify the existing modal tests plus a new regression test for the null rerank section.

## 5. Local serving and documentation

- [ ] 5.1 Add the `vllm-rerank` compose service (`rerank` profile, `Qwen/Qwen3-Reranker-0.6B`, score task, loopback host port 8002, shared HF cache) and the `vllm-rerank` mode in `scripts/dev.sh` + `make dev-vllm-rerank` (log, pid, stop, status parity with the embed target); verify `docker compose config` parses and that a host-run smoke returns a scored `results` entry from `POST /rerank`.
- [ ] 5.2 Update `docs/knowledge-base.md` (what reranking does, the per-user setup, the local `make dev-vllm-rerank` + modal settings, operational knobs, measurement command), `README.md`/`README.zh.md` feature and env lists, and the deployment notes; verify every documented command runs as written and the docs name no deployment-level rerank endpoint.
- [ ] 5.3 Tick the retrieval entry in `FUTURE_BACKEND.md` and reference the measured rerank numbers; verify the roadmap text matches the shipped behavior.

## 6. Measurement and end-to-end gate

- [ ] 6.1 Add the opt-in KB recall rerank measurement (`NYX_KB_RECALL_RERANK_URL` + model) reusing the checked-in corpus, labels, and frozen vectors; run it against the local vLLM reranker and record the per-scenario/per-category recall@1/@5/@10 table in `docs/knowledge-base.md`; verify the default suite still runs with no reranker and the dense floors are unchanged.
- [ ] 6.2 Add a committed Playwright e2e against a side stack with a deterministic rerank stub and a seeded user carrying a rerank group: assert the chat citation order follows the stub's order, then stop the stub and assert dense-order fallback; verify the spec passes with `npm run test:e2e` (or the documented `E2E_STACK_SUFFIX` live variant).
- [ ] 6.3 Run the full gates: `cd backend && make test`, `golangci-lint run --timeout=5m`, `make coverage-check`, `make sqlc-diff`, `make openapi-diff`; `cd frontend && npm run build && npm run lint && npm test && npm run test:coverage`; `cd backend/agent_py && make lint && make test`; and the e2e set. Record any environment-limited skip explicitly.
