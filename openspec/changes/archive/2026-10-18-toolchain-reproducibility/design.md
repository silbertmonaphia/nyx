# Design

## Context

See `proposal.md` — Why. Three build paths exist: local development, GitHub
Actions, and Docker/compose. Application dependency versions are already
lockfile-driven per stack. The remaining drift is at the toolchain and base-image
layer:

- `backend/go.mod` declares `go 1.26.9` (the stdlib security baseline from
  commit `59628f7`); CI pins `setup-go` 1.26.9; the local gvm default is 1.26.1
  and relies on `GOTOOLCHAIN=auto`; the backend Docker build stage still declares
  `golang:1.26.1-alpine` and therefore also downloads a toolchain at build time.
- Frontend dependencies are locked (`frontend/package-lock.json`, v3) and the
  main CI jobs and Docker build already use `npm ci`, but the two e2e jobs use
  `npm install`, the local Node runtime is 24.11.0 while CI and Docker use Node
  22, and no version file declares the intended major. `@types/node` is
  `^25.5.0` (resolved 25.9.9), one major ahead of the runtime.
- `frontend/.dockerignore` exists; `backend/.dockerignore` does not, so the Go
  build context includes `backend/agent_py/.venv` and totals ~482 MB.

Registry access is available through the environment proxy, but can be flaky;
multi-architecture OCI index digests are resolved from the registry API rather
than from a local image (the local store does not retain `RepoDigests`).

## Goals / Non-Goals

**Goals:**

- One declared toolchain version per language across local dev, CI, and images.
- The developer Go toolchain matches `go.mod` without relying on auto-download.
- Node type definitions target the same major as the runtime.
- Digests pinned for every external base/runtime image used by the app images.
- Lockfile-exact dependency installation in every CI lane and image build.
- Build contexts containing only build inputs, no local state or secrets.

**Non-Goals:**

- Pinning apt package versions or the Debian repository snapshot.
- Pinning compose infrastructure images (`postgres`, `redis`, `pgvector`).
- Enforcing Python venv drift inside `make dev`.
- Changing application dependency versions or runtime behavior.

## Decisions

### Node 24 everywhere, not Node 22

The local development environment (the one actually exercised daily) runs Node
24.11.0; CI and Docker are the outliers at Node 22. Node 24 is an LTS line, and
aligning upward avoids forcing the developer onto an older runtime while
removing the only real dev-vs-prod Node mismatch.

The version is declared once in a repository-root `.nvmrc` (`24`). CI reads it
with `node-version-file: .nvmrc`; the frontend Dockerfile spells out
`node:24-alpine@<digest>` because its build context is `./frontend` and cannot
see the root version file. The two must be bumped together; the e2e verification
task covers it.

`engines` is deliberately not added to `frontend/package.json` in this change:
it adds lockfile metadata churn and npm warnings without adding enforcement
beyond what `.nvmrc` + CI/Docker already provide.

Alternative considered: pin Node 22 everywhere (rejected — forces the developer
to downgrade and keeps the older runtime in the production path).

### Node type definitions follow the runtime major

`@types/node` moves from `^25.5.0` to the Node 24 major and the lockfile is
regenerated. Type definitions one major ahead of the runtime let TypeScript accept
calls to APIs that Node 24 does not provide — code that compiles and passes local
tooling but throws at runtime. This is the same dev-vs-prod class this change
targets, at the type-surface layer.

Alternative considered: bump the runtime to Node 25 to match the types (rejected —
25 is an odd, non-LTS line, and the goal is a single declared runtime major, not
tracking the newest types).

### Go builder at 1.26.9, pinned by digest

The build stage becomes `golang:1.26.9-alpine@<digest>`, matching `go.mod` and
CI and removing the implicit `GOTOOLCHAIN=auto` download. The image tag stays
visible for readability; the digest freezes the actual content.

Alternative considered: lower the `go` directive back to 1.26.1 (rejected —
reintroduces the stdlib advisories fixed at 1.26.9 and contradicts the existing
security commit).

### Manual digest pinning for golang, node, and nginx

For each image, resolve the multi-arch OCI index digest from the registry
(`Docker-Content-Digest` for the tag), then reference
`tag@sha256:<digest>` in the Dockerfile. Bumping is an explicit, reviewable
change. Dependabot/Renovate is not configured in this repository and adding it
is out of scope.

Alternative considered: keep moving tags (rejected — a rebuild of the same
commit could silently change the base layer).

### `npm ci` in the e2e lanes

Both e2e jobs switch from `npm install --workspaces=false` to
`npm ci --workspaces=false`, matching the main CI and Docker installs. The
lockfile is committed and the repository already enforces lockfile regeneration
via lint-staged, so a mismatch is an explicit failure rather than a silent
re-resolution.

### Minimal backend build context

Add `backend/.dockerignore` excluding `agent_py/` (a separate service and image),
`.env*`, coverage output, caches, and test artifacts. The Go build stage needs
only `cmd/`, `internal/`, `migrations/`, `docker-entrypoint.sh`, `go.mod`, and
`go.sum`; the agent tree is not referenced by the backend image.

Expected effect: build context drops from ~482 MB to single-digit megabytes and
local environment files never reach the daemon.

## Risks / Trade-offs

- [Node 24 changes build output] → Run `npm ci`, `npm run build`, and `npm test`
  under Node 24 before merging; local development already runs 24, so the
  exposure is limited.
- [Digest pinning hides upstream security updates] → Bumps are explicit and
  reviewable; package-level advisories remain covered by `npm audit`,
  `govulncheck`, and `pip-audit`. Note the bump procedure in the change tasks.
- [Registry/proxy instability during implementation] → Digests are resolved and
  verified once, then committed; image builds no longer depend on tag metadata.
- [`npm ci` fails on lock drift] → This is the intended failure mode; the
  repository already regenerates the lockfile via lint-staged.
- [Local gvm still at 1.26.1] → Task 2.4 upgrades the default gvm toolchain to
  1.26.9 and documents the command; `GOTOOLCHAIN=auto` remains a fallback for
  machines that have not been upgraded. Docker no longer relies on it.

## Migration Plan

Single revertible commit set; no data or API migration. After merge, rebuild the
compose images (`docker compose --profile app build`) and let the existing CI
lanes verify. Rollback is a revert; the previous tags/digests rebuild the
previous images.

## Open Questions

None. Deferred items are explicitly listed as non-goals.
