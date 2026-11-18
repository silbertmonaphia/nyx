# Proposal

## Why

`openspec/changes/rag-rerank/` ships the implementation: the per-user BYOK rerank stage, the settings/API/modal surface, the local vLLM serving target, and the CI-runnable tests. The remaining work needs a deployed stack, a clock, or an operator decision, so it cannot be completed in the dev/CI environment by the implementer.

## What Changes

No code. This change tracks the rollout gates only:

- Deployed-stack smoke with a configured test user.
- Canary quality measurement against the deployed reranker compared with the dense baseline.
- Staging rollback drill (clear the rerank group, verify dense fallback).
- Operator deploy notes for offering a shared reranker and watching its signals.
- Archive by the PM/operator once the gates are checked.

## Capabilities

None. `skip_specs: true` — the rollout has no spec-level behavior; all behavior is specified by `openspec/changes/rag-rerank/`.

## Impact

- Depends on the implementation change `openspec/changes/rag-rerank/` (archive independently when its tasks complete).
- Docs referenced by the gates: `docs/knowledge-base.md` (per-user setup and measurement command), `docs/deployment*.md` (operator serving notes).
- PM/operator ownership: the implementer records evidence and leaves the archive task unchecked, mirroring `openspec/changes/agent-production-rollout/`.
