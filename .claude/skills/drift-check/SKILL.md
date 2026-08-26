---
name: drift-check
description: Run the four backend drift checks husky doesn't cover — gofmt, sqlc, openapi, golangci-lint — that are easy to forget and CI catches after the fact. Use when the user says "ready to commit", "ready to push", "pre-push", "is this safe to push", "drift check", "anything stale?", or asks to validate a backend change before pushing — even if they don't name the check explicitly. Skips frontend-only commits.
---

# Drift Check

Catches the backend artifacts that drift silently and that CI flags after push.

## Usage

```bash
bash .claude/skills/drift-check/scripts/drift-check.sh
```

Exit: `0` if no backend files were touched or all checks passed; `1` otherwise.

## What it does

- Detects whether `backend/` was touched (working tree + staged + vs-base). Frontend-only commits exit 0 immediately.
- Runs four checks, in order:
  1. **format** — `gofmt -l` over `backend/**/*.go`. Lists unformatted files on failure.
  2. **drift:sqlc** — `make sqlc-diff`. Fails if `queries/*.sql` or a migration changed without regenerating `internal/{movie,user}/db`.
  3. **drift:openapi** — `make openapi-diff`. Fails if a handler changed without regenerating `api/openapi.json`.
  4. **lint** — `golangci-lint run --timeout=5m`.
- Aggregates pass/fail. Each failure prints the first 30 lines of its output for fast triage.

Husky already runs `eslint --fix` + `vitest related` on commit — those gates are intentionally not duplicated here.