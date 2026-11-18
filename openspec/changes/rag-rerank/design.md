# Design

## Context

See `proposal.md` — Why. Today `rag.Service` owns the retrieval path end to end: it resolves the caller's BYOK embedder, embeds the query, calls `Retriever.Retrieve` (feeds) or `kb.Service.Retrieve` (KB) with the requested `k` (`RAG_TOP_K` / `KB_TOP_K`, default 5), and renders the context block. The agent bridge calls the same `RetrieveCorpus` with `corpus=feeds|kb|both`; `both` fuses the two lists by Reciprocal Rank Fusion (constant 60). Per-user BYOK is strict: a user's chat, embedding, and Tavily endpoints come from `user_llm_settings` and no server env fallback applies on the user path. The measured baselines (KB: 26 docs / 89 chunks / 64 labelled queries, recall@1 = 0.836, recall@5 = 0.984; feeds: recall@5 = 0.944, mrr@5 = 0.905) show the remaining errors are rank crowding, not recall gaps — `FUTURE_BACKEND.md` records this as a rerank/top-k problem.

Local dev serves vLLM chat on :8000 and the embedding model on :8001; compose pins `vllm/vllm-openai:v0.29.0` and the host loop uses vLLM 0.17.1, which exposes the Jina/Cohere-compatible `POST /rerank` (and the `/v1/rerank` alias) for the `score` task and has a Qwen3-Reranker config hook.

## Goals / Non-Goals

**Goals:**

- One rerank seam in `rag.Service` shared by the chat path, the agent bridge, and every corpus mode.
- Per-user BYOK semantics consistent with the rest of the product: the user's endpoint, key, and model, encrypted at rest, no deployment fallback.
- Deterministic, testable ordering with no live model in the default CI suite.
- Dense order remains the safe fallback for an absent, broken, or slow reranker.

**Non-Goals:**

- No lexical/BM25 hybrid leg, no score-threshold calibration, no index or chunker changes.
- No deployment-level reranker endpoint; the reranker is BYOK-only.
- No rerank for the indexer or backfill — retrieval only.
- No score exposure on the wire; the reranker changes order, not the citation envelope.
- No agent-side rerank; the Python agent keeps consuming the bridge unchanged.

## Decisions

### D1 — The stage lives in `rag.Service`, resolved per caller

`rag.Service` already owns embed → retrieve → render for both corpora and both callers. Add a `RerankerResolver func(ctx, userID int) (Reranker, error)` mirroring the existing `EmbedderResolver`, plus a `Reranker` interface (`Rerank(ctx, query string, docs []string) ([]float64, error)`). The resolver returns `nil` when the user has no rerank group; the service then skips the stage entirely. The per-user client is built in `llmsettings.Resolver` from the user's stored group.

Alternatives considered and rejected: rerank inside `kb.Service` (leaves feeds out and duplicates the stage), rerank in the Python agent (the bridge exists so retrieval lives in one place; `rag:true` chat would stay unranked), and a deployment-level reranker (violates strict BYOK: private passage text must only go to endpoints the user configured).

### D2 — Over-fetch `RERANK_CANDIDATES` (default 20), return the requested count

`candidateK = max(RERANK_CANDIDATES, requestedK)`. The existing ANN SQL already takes `k`, so no query or index change. Reranking only the same 5 cannot recover the measured rank 6–10 misses; 20 gives headroom at negligible extra vector-search cost. The deployment bound is a startup-validated integer.

### D3 — Per-corpus rerank, then RRF

Each corpus is reranked independently on its own candidate list; `both` then fuses the two post-rerank lists with the existing RRF. This preserves the "fusion is by rank" contract and avoids comparing reranker scores across different passage shapes and models. Reranking the already-fused list was rejected: RRF scores are not relevance scores and the fused list mixes corpora before relevance is measured.

### D4 — vLLM `/rerank` wire contract, dedicated client

Request: `POST {rerank_base_url}/rerank` with `{"model": ..., "query": ..., "documents": ["...", ...], "top_n": <len>}` and `Authorization: Bearer <key>` only when a key exists. Response: `{"results": [{"index": n, "relevance_score": x, ...}], ...}`; only `index` and `relevance_score` are consumed, and echoed document text is ignored. The base URL is the endpoint prefix — the client appends `/rerank` — so vLLM (`http://localhost:8002`), Jina (`https://api.jina.ai/v1`), and Cohere (`https://api.cohere.com/v2`) all fit without code changes.

Why a dedicated client instead of extending `llm.Provider`: `Provider` is the chat/embed seam with router failover semantics; a third verb would force every provider implementation and the router to grow it. The client is stdlib `net/http`, mirroring the existing vLLM embed client's timeout/error handling, so no new dependency. Response parsing uses a captured vLLM body fixture checked against the protocol schema (a producer-only fixture would only prove agreement with itself).

### D5 — Ordering and tie-breaks are deterministic and defensive

Sort by reranker score descending, then dense rank ascending. The response is treated as untrusted: out-of-range and duplicate indices are ignored, each candidate is scored at most once, unscored candidates keep dense order at the tail, and no index can introduce a passage outside the candidate pool. `Passage.Score` stays the dense cosine similarity; the reranker changes order only.

### D6 — Failure policy: one bounded attempt, then a per-configuration cooldown

`RERANK_TIMEOUT` defaults to 2s. A timeout, connection error, non-2xx, or malformed body logs a warning, records a fallback outcome, and returns dense order; the retrieval never fails. There is no retry — retrieval is on the chat critical path and a retry doubles the worst-case wait for an optional stage. After 3 consecutive failures for a given rerank configuration, calls are skipped for 30s, then a single probe attempt decides whether to resume. The breaker is in-process and keyed by the configuration (base URL + model + key hash), so one user's outage cannot suppress another user's reranking. A shared external circuit breaker was rejected as unnecessary machinery for a latency guard.

### D7 — BYOK plumbing: fourth optional group in `user_llm_settings`

- Migration `000022` adds `rerank_base_url`, `rerank_api_key_enc`, and `rerank_model`, all `TEXT NOT NULL DEFAULT ''`; down drops them. No backfill: every existing user keeps dense ordering.
- Settings/repository: the three fields join the row struct, the encryption/decryption path, and the GET response as a `rerank` section. GET masks the key tail-4, like every other key.
- PUT: rerank is all-or-nothing on base URL and model; a loopback base URL may omit the key; a remote base URL requires a key unless one is stored (empty update means keep); URLs pass the same http/https, host, and no-userinfo validation; placeholder keys are rejected; a rerank-only configuration satisfies "at least one group".
- The embedding model's immutability does not apply: changing the rerank model or endpoint has no index implications and is allowed freely.
- Resolver: `UserStack` gains the rerank fields and a rerank client built through the same SSRF transport (`BYOK_ALLOW_PRIVATE_URL`) and the same small LRU cache keyed by slot values.
- The rerank group is never forwarded to the Python agent; retrieval and reranking run in Go.
- Probe: `POST /api/me/llm-config/validate` gains a rerank slot that sends one short query and one short document to the same path; success requires a 2xx with a parseable scored result, and the existing status classification applies.

### D8 — Rerank text mirrors the embedded text

Feeds use `ChunkText` (`Title + "\n\n" + Description`) — exactly what the embedder saw. KB reconstructs the chunk prefix the normalizer embedded (`rel_path › heading / path\n\n` + body) through a small exported helper, because the prefix function is currently unexported in the chunker. Including provenance keeps path- and heading-scoped queries fair and gives the reranker the same evidence the dense leg had. Each text is capped at `RERANK_MAX_DOC_CHARS` (default 4000) on a UTF-8 boundary.

### D9 — Local serving mirrors the embed service

`docker-compose.yml` gains a `vllm-rerank` service under a `rerank` profile: the pinned vLLM image, `VLLM_RERANK_MODEL` defaulting to `BAAI/bge-reranker-v2-m3`, the `pooling` runner so `/rerank` is mounted, host port 8002 (loopback only), `--max-model-len 4096`, `--max-num-seqs 32`, `--enforce-eager`, the shared HF cache, and the same GPU reservation. `scripts/dev.sh` and the `Makefile` gain `vllm-rerank` / `make dev-vllm-rerank` with the same log, pid, stop, and status conventions as the embed target. A developer then configures their own rerank group with base URL `http://localhost:8002`, no key, and the model id.

Implementation smoke (2026-11-18, host vLLM 0.17.1): `Qwen/Qwen3-Reranker-0.6B` does not serve correctly from its base HF checkpoint — vLLM resolves it as `Qwen3ForCausalLM`, `--convert classify` fails on the missing `score.weight`, and `--convert embed` scores badly (measured recall@1 0.297 on the KB corpus). `BAAI/bge-reranker-v2-m3` serves natively as `XLMRobertaForSequenceClassification` with `--runner pooling`; a 20-document batch returns scored results and the KB rerank measurement gives dense 20→10 recall@1/@5 = 0.836/0.992 versus rerank 0.820/1.000 (first rank 1.234 vs 1.172; exact-term and multi-relevant improve, paraphrase recall@1 drops). The default model is therefore BAAI, with the failure recorded here and in `docs/knowledge-base.md`.

GPU budget on the dev box (RTX 5080 Laptop, 16303 MiB). `--gpu-memory-utilization` is a per-process vLLM target, not a hard partition, so three services must budget sum ≤ 1.0 plus WSL2 overhead (CUDA context and pin memory sit outside vLLM's accounting):

| Runtime | chat | embedding | rerank | Total |
|---|---|---|---|---|
| Host (`make dev-vllm-*`) | 0.75 (was 0.90) | 0.10 | 0.15 | 1.00 |
| Compose (`--profile vllm --profile embed --profile rerank`) | 0.70 (unchanged) | 0.15 (unchanged) | 0.15 | 1.00 |

The host chat fraction drops from 0.90 because the current host pair already sums to 1.00; 0.75 leaves the 4B-FP8 model ample room (weights + KV ≈ 7 GB). The reranker gets more than the embedder's 0.10 because it scores up to 20 (query, document) pairs at 4096 tokens, so its activation peak is higher; 0.10 (≈1.63 GB) is too tight for 0.6B weights plus WSL2 overhead. Host runs set `VLLM_WSL2_ENABLE_PIN_MEMORY=1`. OOM fallback: chat 0.72 or rerank 0.12. Verify with `nvidia-smi` after all three start and leave ≥1 GB headroom.

Model risk: the Qwen3-Reranker needs vLLM's original-reranker config path; the smoke test must verify it serves on the installed version. If it does not, `BAAI/bge-reranker-v2-m3` is the registry-native fallback — the wire contract is model-agnostic, and the served model is recorded in this design and the docs. Running all three vLLM services simultaneously is tight on a 16 GB card; the profile is opt-in and the operator sizes it.

### D10 — Testing strategy

- Backend unit: client request/response shape via `httptest` (captured-body fixture + schema oracle), ordering/ties/duplicate/out-of-range/missing indices, fallback and cooldown, startup bounds, settings PUT validation, encryption/masking, resolver per-slot construction, probe classification.
- Backend integration: `rag.Service` with an `httptest` reranker through `RetrieveCorpus` for `feeds`, `kb`, and `both`, plus the bridge handler asserting reordered citations with an unchanged envelope; the KB recall suite stays dense-only and untouched.
- Frontend unit: modal renders the rerank section, saves, masks keys, test-connection states, i18n en/zh parity; generated OpenAPI types updated.
- E2E: a committed Playwright spec against a side stack (`E2E_STACK_SUFFIX` convention) whose backend points at a deterministic rerank stub, with a seeded user carrying a rerank group; assert the chat citation order differs from dense order and that stopping the stub falls back to dense order. This satisfies the mandatory e2e gate.
- Measurement: an opt-in KB recall run (`NYX_KB_RECALL_RERANK_URL`) against the local model records recall@1/@5/@10 per scenario in `docs/knowledge-base.md`; the default CI suite remains model-free.
- Smoke gate: serve the model on the host, call `/rerank` with a real query and documents, then run the e2e before the change is considered done.

## Risks / Trade-offs

- **A cross-encoder can demote exact-match gold** → keep rerank text identical to embed text, measure per category with the opt-in run, and keep the group optional so a deployment can leave it unset. If a category regresses, record it rather than hiding it.
- **Added chat latency** → 2s cap, no retry, cooldown after repeated failures, and a dense-order fallback.
- **Model serving mismatch on the pinned/installed vLLM** → config-swappable model, `bge-reranker-v2-m3` fallback, smoke test before wiring the default.
- **Larger API surface (14 fields, new modal section, probe slot)** → additive migration with defaults, regenerated OpenAPI/types, and frontend unit tests for save/mask/probe/i18n.
- **BREAKING-ish probe shape** → the validate endpoint's body gains an optional section; existing clients that omit it are unaffected. GET responses add a null `rerank` section; consumers must tolerate the additive field (the frontend is regenerated in the same change).
- **Hosted rerank providers can bill per probe** → one tiny probe request, existing per-user rate limit, no caching.
- **Breaker hides a persistently failing reranker** → fallback counters, span status, and operator docs name the signal.
- **VRAM pressure with three vLLM services** → opt-in profiles, documented sizing.

## Migration Plan

- Migration `000022` adds three `NOT NULL DEFAULT ''` columns; existing rows mean "no rerank group", so behavior is unchanged until a user saves one. Down drops the columns.
- Rollback: clear the rerank group (instant, per user) or run the down migration; no vectors or documents depend on the reranker, so no re-index is needed.
- Deploy order (production): apply migration with the regular backend boot → ship the backend + SPA → operators may point users at a shared reranker. Rollout gates live in `openspec/changes/rag-rerank-rollout/`.

## Open Questions

- None blocking. The candidate count (20) and timeout (2s) are deployment config; the smoke and opt-in measurement recorded in `docs/knowledge-base.md` are the tuning baseline. The local default model is `BAAI/bge-reranker-v2-m3` because the Qwen3-Reranker base checkpoint cannot be served by the available vLLM classifier path; the wire contract stays model-agnostic so an operator can swap the model or endpoint per user.
