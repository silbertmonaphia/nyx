# Proposal

## Why

Dense retrieval is strong at recall@5 but weak at top-1: the hardened KB baseline measures recall@5 = 0.984 with recall@1 = 0.836, and the feed baseline's only remaining misses are gold chunks crowded to ranks 6–10 — recorded in `FUTURE_BACKEND.md` as "a rerank/top-k problem, not a lexical-recall one". A bi-encoder scores query and passage independently, so it cannot separate paraphrases that sit at the same cosine distance; a cross-encoder reading the pair can. A reranker over an over-fetched candidate pool is the smallest change that raises first-result fidelity without touching the index, chunker, or embedding identity.

## What Changes

- Add a BYOK-configured rerank stage to the Go RAG service, applied to both corpora and every retrieval caller (`rag:true` chat, `corpus=feeds|kb|both` on the agent bridge):
  - Over-fetch `RERANK_CANDIDATES` (default 20) passages from the existing ANN query, score them with a cross-encoder, and return the requested `top_k` in reranker order.
  - `both` reranks each corpus independently before the existing Reciprocal Rank Fusion, so fusion stays rank-based.
  - A user with no rerank configuration keeps today's dense-only result, unchanged.
- Extend the per-user BYOK configuration with an optional `rerank` group (base URL, API key, model), following strict BYOK: no deployment-level fallback on the user path. Stored encrypted in `user_llm_settings` (migration `000022`), masked on GET, validated on PUT, invoked with the user's own endpoint.
- Serve the reranker through vLLM's Jina/Cohere-compatible `POST /rerank`; default local/dev model `BAAI/bge-reranker-v2-m3` (a native cross-encoder). `Qwen/Qwen3-Reranker-0.6B`'s base HF checkpoint needs a converted classifier head vLLM cannot build, so it is not the default. New `vllm-rerank` compose profile and `make dev-vllm-rerank` host target mirror the embed service, so local users point their BYOK rerank group at `http://localhost:8002`.
- Deployment-level operational bounds only: `RERANK_CANDIDATES` (default 20), `RERANK_TIMEOUT` (default 2s), `RERANK_MAX_DOC_CHARS` (default 4000); the endpoint, key, and model are per-user.
- Reranking is best-effort: timeout or upstream failure falls back to dense order and never fails retrieval or chat; consecutive failures open a short cooldown so a down reranker does not add its full timeout to every request.
- Config modal gains a collapsible Rerank section in both languages, with guidance, a red impact warning, and an independent test-connection control; the config validate endpoint gains a rerank slot.
- The default recall baseline stays dense-only and model-free; an opt-in rerank measurement reuses the checked-in KB corpus and reports the same metric family against a live reranker.
- Documentation: `docs/knowledge-base.md`, `.env.example` operational knobs, deployment notes, README env/feature lists, and the `FUTURE_BACKEND.md` retrieval entry.

## Capabilities

### New Capabilities

- `rag-rerank`: the second retrieval stage — candidate over-fetch, per-corpus rerank before fusion, deterministic ordering, failure and cooldown degradation, upstream-response validation, telemetry, and the per-user enablement rule.

### Modified Capabilities

- `kb-retrieval`: the feed-corpus-unaffected requirement gains a rerank carve-out (feed order may change when the caller has reranking configured), and the fusion requirement states that fusion consumes each corpus's post-rerank order.
- `agent-rag-bridge`: the frozen feed-retrieval shape is scoped to corpus selection and citation shape; passage order may differ when the rerank stage is enabled.
- `kb-recall-eval`: the default run remains dense-only; a new opt-in rerank measurement reuses the checked-in corpus and reports the same metrics.
- `user-llm-config`: a fourth optional group joins the persisted/encrypted settings, CRUD shape, PUT validation, per-slot resolution, strict-BYOK gating, modal guidance, connectivity probe, and probe classification.

## Impact

- Code: `backend/internal/rag/` (reranker client, per-user resolver, ordering stage), `backend/internal/llmsettings/` (settings model, repository, resolver, probe, PUT validation), `backend/internal/kb/` (rerank-text helper), `backend/internal/platform/config/` (operational knobs), `backend/cmd/api/main.go`; migration `000022`; `backend/queries/llmsettings.sql` + sqlc regenerated.
- API: `GET/PUT/DELETE /api/me/llm-config` and `POST /api/me/llm-config/validate` gain a `rerank` section; OpenAPI + generated frontend types regenerated. Chat and agent wire shapes are unchanged.
- Frontend: `LlmConfigModal` gains a collapsible Rerank section, test-connection control, i18n (en/zh), and tests.
- Infra: `docker-compose.yml` gains a `vllm-rerank` service under a `rerank` profile; `scripts/dev.sh` + `Makefile` gain the host-run target; operators may point users at any Jina/Cohere-compatible endpoint.
- Data flow: candidate passage text (user-private content) is sent to the user's own configured reranker endpoint, the same trust domain as that user's embedding endpoint; no new deployment-level recipient of private content.
- Tests: backend unit (client, ordering, fallback, cooldown, PUT validation, probe), integration (`httptest` reranker through `rag.Service` and the bridge), frontend unit (modal + i18n), opt-in live rerank measurement, Playwright e2e against a side stack with a deterministic reranker stub. The mandatory e2e gate is satisfied by the side-stack spec.
- Rollout gates tracked in `openspec/changes/rag-rerank-rollout/` (serving a shared/dev reranker, configured-user canary measurement, rollback by clearing the group, deployed-stack smoke).
