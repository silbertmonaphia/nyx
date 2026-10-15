# Proposal

## Why

Two hygiene gaps surfaced while landing `ci-redis-and-py-vuln-gates`:

1. `backend/test/testcontainers.go` hardcodes `postgres:17-alpine`, which
   ships no `vector` extension, so the local Docker-mode `go test ./...`
   path fails `internal/feed`'s migrations while CI passes (CI sets
   `DB_URL` and runs against the pgvector service container). The local loop
   is what the project's e2e gate depends on; a path that is red by
   construction trains everyone to ignore it.
2. `.github/workflows/ci.yml` has no workflow-level `permissions:` default —
   only the `release` job declares a block. Every other job therefore runs
   with the repository's default token scope, and the `py-audit` job added by
   the previous change installs and executes repository code inside it.

## What Changes

- `backend/test/testcontainers.go`: the container image becomes
  `pgvector/pgvector:pg17` — the same image CI's `backend-test` service
  already uses — so the helper's Postgres can apply the full migration set
  including `CREATE EXTENSION vector`.
- `.github/workflows/ci.yml`: add a workflow-level `permissions:
  contents: read`. The `release` job keeps its job-level block
  (`contents: write`, `issues: write`, `pull-requests: write`), which
  overrides the default, so tag and release creation are unaffected.
- No runtime code, no dependency, no new action pin, no migration.

## Capabilities

None. This is test-infrastructure and CI hygiene with no requirement-level
behavior change, so the change carries no spec delta and declares
`skip_specs: true` in `.openspec.yaml` (the schema's rule for zero-delta
changes).

## Impact

- `backend/test/testcontainers.go` — one image string.
- `.github/workflows/ci.yml` — two lines at the top level.
- CI behavior is unchanged in effect: the testcontainer helper is not used
  where `DB_URL` is set, and every non-`release` job only reads the repo.
- No rollout gates — completable in dev/CI; rollout-split sibling not
  required.
