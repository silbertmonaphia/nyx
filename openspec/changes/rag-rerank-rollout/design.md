# Design

## Context

See `proposal.md` — Why, and `openspec/changes/rag-rerank/design.md` for the implementation. The rerank stage is per-user BYOK and default-off: the code ships with no user configured, so retrieval is dense-only until a user saves a rerank group. That makes the rollout two questions rather than one: is the code safe to deploy (migration and fallback behavior), and should the operator publish a shared reranker for users to point at.

## Goals / Non-Goals

**Goals:**

- Deploy the migration and code without changing any existing user's retrieval.
- Prove the fallback path on a deployed stack before recommending a reranker.
- Decide, from a measured canary, whether to publish a shared reranker.
- Keep rollback to a configuration action, not a data migration.

**Non-Goals:**

- Serving a production GPU for the reranker in this change (an operator decision).
- Changing the implementation's specs or code.
- Making the rerank group mandatory for any user.

## Decisions

### D1 — Deploy order: migration and code first, reranker guidance second

The backend boot applies `000022`; because every column defaults to empty, no user's retrieval changes. The SPA ships in the same deploy. Only after the deployed smoke passes does the operator publish a shared reranker and the guidance pointing users at it.

### D2 — Canary measures the same corpus as CI

The canary reuses the checked-in KB recall corpus and the opt-in measurement from the implementation change against the deployed reranker, so the comparison with the dense baseline is apples-to-apples. A production corpus measurement is optional and separate; it must not replace the checked-in table.

### D3 — Rollback is per-user configuration

Clearing a user's rerank group immediately returns that user to dense order; no vectors, documents, or backfill depend on the reranker. The down migration is available but unnecessary for a quality rollback. This is the drill the staging gate exercises.

## Risks / Trade-offs

- **A deployed reranker is slower than the local one** → the implementation's 2s cap and cooldown bound the exposure; the canary records observed latency and fallback rate before any announcement.
- **Users point at incompatible rerank APIs** → the modal guidance pins the Jina/Cohere-compatible `POST /rerank` shape and the probe classifies failures before save.
- **Shared GPU sizing** → the operator notes state the VRAM cost beside chat and embedding and make the shared service optional.
- **Quality regressions on some categories** → the canary records per-category numbers; the operator decides whether to publish or restrict guidance.

## Migration Plan

1. Deploy backend (migration applies) + SPA; verify existing users show `rerank: null` and retrieval is unchanged.
2. Run the deployed-stack smoke with a test user.
3. Optionally publish the shared reranker and point the test user at it.
4. Run the canary measurement; record results.
5. Rollback, if needed: clear the group (per user) or disable the shared endpoint; no data action.
