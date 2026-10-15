# Proposal

## Why

`frontend/src/api/openapi/schema.d.ts` is generated from `api/openapi.json`
(`npm run openapi:gen`), but nothing in CI compares the committed copy with
the spec. It silently fell behind: at `HEAD` the file was missing the
`get-feed` operation that `api/openapi.json` has carried since commit
`32e20a5`, so the SPA's typed client described an older API than the server
shipped. The backend half of this drift is already gated (`make openapi-diff`
in `backend-test`); the frontend half has no gate, and the fresh instance
fixed by `ci-redis-and-py-vuln-gates` will rot again without one.

## What Changes

- `frontend-test` gains a step — `cd frontend && npm run openapi:check` —
  which regenerates `schema.d.ts` from `api/openapi.json` into a temp file
  and `diff -u`s it against the committed copy.
- A diff fails the job and prints the regenerated lines, so the author runs
  `npm run openapi:gen` and commits the result.
- No new dependency and no runtime change: the `openapi:check` script and
  the `openapi-typescript` devDependency already exist, and the job already
  installs with `npm ci`, so the step costs ~2 s.
- The gate covers the committed artifact only. The reverse direction — the
  SPA calling a field the spec does not declare — stays out of scope (that
  is the type-system's job at build time), as does re-checking
  `api/openapi.json` against the handlers (`make openapi-diff` owns it).

## Capabilities

### New Capabilities

- `ci-openapi-typecheck-gate`: the merge gate that fails when the committed
  frontend OpenAPI types drift from `api/openapi.json`.

### Modified Capabilities

- None. The generator's output, the script, and the backend's
  `make openapi-diff` behavior are untouched — this change only adds the
  missing CI step.

## Impact

- `.github/workflows/ci.yml` — one step in `frontend-test` (the job already
  sets up Node and runs `npm ci`, so no new setup cost).
- `frontend/package.json` — unchanged (`openapi:check` exists).
- No runtime code, no migration, no new dependency.
- No rollout gates — completable in dev/CI; rollout-split sibling not
  required.
