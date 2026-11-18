# Tasks

> Post-implementation gates for `openspec/changes/rag-rerank/`. The implementation change archives independently when its tasks complete; this change stays active until the gates below are checked by the PM/operator.

## 1. Deployed-stack smoke

- [ ] 1.1 Deploy the backend + SPA and confirm the `000022` migration applied on boot (rerank columns present, existing users report `rerank: null`).
- [ ] 1.2 Configure a test user's rerank group against the deployment's reranker (base URL, model, key as applicable; the local/dev reference is `BAAI/bge-reranker-v2-m3` on port 8002 with `--runner pooling`) and verify `POST /api/me/llm-config/validate` reports `ok` for the rerank slot.
- [ ] 1.3 From the SPA chat and from the agent path (`corpus=both`), verify citations are reordered relative to a recorded dense-order control, with the citation envelope unchanged.
- [ ] 1.4 Clear the test user's rerank group and verify the next retrieval returns dense order with no reranker call and no error.

## 2. Canary quality measurement

- [ ] 2.1 After a CI-green window, run the opt-in KB recall rerank measurement against the deployed reranker with the production corpus; record per-scenario and per-category `recall@1`/`recall@5`/`recall@10` alongside the dense baseline in `docs/knowledge-base.md`.
- [ ] 2.2 Compare against the dense baseline; if a category regresses, record it and decide whether to keep the rerank group published or restrict the guidance before announcing the feature.

## 3. Rollback drill (staging)

- [ ] 3.1 Simulate a reranker outage (stop the endpoint or point the test user at a dead URL) and verify retrieval degrades to dense order, chat still completes, and the fallback metric increments.
- [ ] 3.2 Verify the recovery path after the cooldown (probe succeeds, reranking resumes) and record the observed latency added by the cooldown window.
- [ ] 3.3 Confirm rollback needs no data action: clearing the group (or running the down migration) restores dense retrieval with no re-index.

## 4. Operator handoff

- [ ] 4.1 Publish the operator section for offering a shared reranker: serving command (`vllm serve BAAI/bge-reranker-v2-m3 --runner pooling --port 8002`, or a Jina/Cohere-compatible vendor endpoint), VRAM sizing beside chat + embedding, the per-user base URL/model to hand out, and the Prometheus/Jaeger signals for rerank success/fallback/cooldown. Record that the local measurement showed recall@5 0.992 → 1.000 but recall@1 0.836 → 0.820 on the synthetic KB corpus, so the publish decision is quality-guided, not automatic.
- [ ] 4.2 Archive this change once 1.x–4.1 are checked (PM/operator only).

## References

- `openspec/changes/rag-rerank/` — the implementation change (specs, design, tasks).
- `docs/knowledge-base.md` — per-user setup, measurement command, measured tables.
- `docs/deployment.md` / `docs/deployment-selfhost.md` — deployment and operator notes.
