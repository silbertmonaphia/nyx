# Tasks

## 1. Resolve image digests

- [x] 1.1 Resolve the multi-arch OCI index digest for `golang:1.26.9-alpine`,
  `node:24-alpine`, and `nginx:stable-alpine` through the registry manifest API;
  verify each response reports an OCI image index media type containing
  `linux/amd64`.

## 2. Backend build reproducibility

- [x] 2.1 Change the backend Dockerfile build stage to
  `golang:1.26.9-alpine@<digest from 1.1>`; verify with
  `docker build --target build -t nyx-go-builder backend` followed by
  `docker run --rm nyx-go-builder sh -c 'go version && go env GOVERSION'`,
  which must report `go1.26.9`, and confirm the build log contains no
  `golang.org/toolchain` download.
- [x] 2.2 Add `backend/.dockerignore` excluding `agent_py/`, `.env*`, coverage
  output, caches, and test artifacts; verify `DOCKER_BUILDKIT=0 docker build
  backend` reports a build context under 10 MB and `docker build --check
  backend` reports no warnings.
- [x] 2.3 Update the project `CLAUDE.md` repo-shape line from Go 1.26.1 to
  1.26.9; verify `grep -n "Go 1.26" CLAUDE.md` reports only 1.26.9.
- [x] 2.4 Align the developer machine's Go toolchain with `go.mod` using gvm
  (`gvm install go1.26.9 && gvm use go1.26.9 --default`) and document the
  command in `CLAUDE.md`; verify `go version` and
  `cd backend && go env GOVERSION` both report go1.26.9.

## 3. Frontend toolchain alignment

- [x] 3.1 Add a repository-root `.nvmrc` containing `24`; verify `cat .nvmrc`
  prints `24` and the local `node -v` major is 24.
- [x] 3.2 Change the frontend Dockerfile build stage to `node:24-alpine@<digest
  from 1.1>` and the runtime stage to `nginx:stable-alpine@<digest from 1.1>`;
  verify `docker build frontend` succeeds, `docker run --rm <image> nginx -v`
  prints a version, and no unpinned `node:` or `nginx:` reference remains.
- [x] 3.3 Change every `setup-node` block in `.github/workflows/ci.yml` from
  `node-version: '22'` to `node-version-file: .nvmrc`; verify `grep -n
  "node-version" .github/workflows/ci.yml` shows no `'22'` and
  `python3 -c "import yaml; yaml.safe_load(open('.github/workflows/ci.yml'))"`
  exits 0.
- [x] 3.4 Change both e2e jobs from `npm install --workspaces=false` to
  `npm ci --workspaces=false`; verify `grep -n "npm install"
  .github/workflows/ci.yml` returns no matches.
- [x] 3.5 Set `@types/node` to the Node 24 major in `frontend/package.json`
  (`^24`), regenerate `frontend/package-lock.json` with
  `npm install --workspaces=false --package-lock-only`, and verify the resolved
  `@types/node` major equals 24.
- [x] 3.6 Run `cd frontend && npm ci && npm run build && npm test` under Node
  24; verify all three commands exit 0.
- [x] 3.7 Document the Node 24 requirement, `.nvmrc` usage, and the
  install-command split (`npm ci` for clean installs, `npm install` only when
  changing dependencies) in the project `CLAUDE.md` and `README_FULL.md`;
  verify the documented major matches `.nvmrc` and the README no longer
  presents `npm install` as the default clean-install command.

## 4. Integration verification

- [x] 4.1 Rebuild both service images through compose
  (`docker compose --profile app build backend frontend`); verify both builds
  succeed with the pinned digests and record the backend context size.
- [x] 4.2 Verify the agent path is untouched: `cd backend/agent_py && uv lock
  --check` passes and `git diff --stat` lists no file under
  `backend/agent_py/`.
- [x] 4.3 Run the backend's standard checks (`cd backend && go build ./...`
  and `SKIP_CONTAINERS=true go test ./...`); verify both exit 0.
