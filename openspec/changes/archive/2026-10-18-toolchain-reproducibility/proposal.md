# Proposal

## Why

Application dependency versions are now locked per stack — Python agent via
`uv.lock`, Go via `go.mod`/`go.sum`, frontend via `frontend/package-lock.json` —
but the toolchains and base images that *build* those artifacts still float.
Four concrete drifts remain:

- Local frontend development runs Node 24 while CI and the Docker build use
  Node 22; there is no `.nvmrc` or `engines` declaration.
- The backend Docker build stage uses `golang:1.26.1-alpine` while `go.mod`
  requires `go 1.26.9` (the stdlib security baseline from commit `59628f7`),
  so every image build silently downloads a toolchain through `GOTOOLCHAIN=auto`.
- The e2e CI jobs use `npm install` instead of `npm ci`, weakening the lockfile
  guarantee those lanes depend on, and the frontend runtime image
  (`nginx:stable-alpine`) is an unpinned moving tag.
- `backend/` has no `.dockerignore`, so its build context is ~482 MB and includes
  `backend/agent_py/.venv` (a different service's environment).

These do not change application package versions, but they can change compiler
and build output, make builds network-dependent, or fail when the toolchain
cannot be downloaded. Closing them is low-cost and completes the reproducibility
work started by the dependency locks.

## What Changes

- Pin the backend Docker build stage to `golang:1.26.9-alpine@sha256:<digest>`,
  matching the `go 1.26.9` directive and CI's `setup-go`.
- Align Node to 24 across local development, CI, and Docker: add a root `.nvmrc`,
  bump CI `setup-node` from 22 to 24, and switch the frontend build image to
  `node:24-alpine@sha256:<digest>`.
- Switch both e2e CI jobs from `npm install --workspaces=false` to
  `npm ci --workspaces=false`.
- Pin the frontend runtime image `nginx:stable-alpine` by digest.
- Add `backend/.dockerignore` excluding `agent_py/`, `.env*`, cache/coverage
  artifacts, and test output so the Go build context contains only what the
  image needs.
- Align `@types/node` with the declared Node major (currently `^25.5.0`, resolved
  25.9.9, while the runtime is Node 24) and regenerate the frontend lockfile.
- Record the local Go toolchain upgrade to 1.26.9 (gvm) in project documentation
  so the developer toolchain matches `go.mod` instead of relying on
  `GOTOOLCHAIN=auto`.

Out of scope: pinning apt package versions, pinning compose infrastructure
images (`postgres`, `redis`, `pgvector`), and enforcing Python venv drift inside
`make dev`. No runtime application behavior, API contract, or dependency version
changes.

## Capabilities

### New Capabilities

- `build-reproducibility`: deterministic toolchain, base-image, and lockfile
  selection across local development, CI, and container builds.

### Modified Capabilities

None. `ci-supply-chain-gates` covers vulnerability gates and real-Redis tests;
this change adds a distinct build-environment contract rather than changing an
existing requirement.

## Impact

- `backend/Dockerfile`, `backend/.dockerignore` (new)
- `frontend/Dockerfile`, `frontend/package.json`, `frontend/package-lock.json`
- `.nvmrc` (new)
- `.github/workflows/ci.yml` (e2e install steps and `setup-node` versions)
- `CLAUDE.md` (Go version and local toolchain guidance)
- No production rollout gates: every verification runs in the dev/CI
  environment, so no sibling `toolchain-reproducibility-rollout` change is
  created.
