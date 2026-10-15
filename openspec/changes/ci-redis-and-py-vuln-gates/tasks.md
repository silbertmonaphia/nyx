# Tasks

## 1. Dependencies

- [ ] 1.1 Add `github.com/testcontainers/testcontainers-go/modules/redis` to `backend/go.mod` (run `go get github.com/testcontainers/testcontainers-go/modules/redis@<pinned>`) and verify `cd backend && go mod vendor` exits 0 and `go.sum` gains only that module's lines
- [ ] 1.2 Add `pip-audit` to `backend/agent_py/pyproject.toml` `[project.optional-dependencies].ci` (or equivalent dev-deps group) and verify a fresh `python -m venv .venv && .venv/bin/pip install -e .[ci]` installs `pip-audit` and `pip-audit --help` exits 0

## 2. Real-Redis integration suite

- [ ] 2.1 Create `backend/internal/platform/cache/cache_integration_test.go` with a `TestMain` that starts one shared Redis testcontainer (skip on `SKIP_CONTAINERS=true` or empty `dbURL`, mirroring `feed/repository_integration_test.go:34,98-100`) and verifies `go test ./internal/platform/cache/...` runs and the container start log shows `redis:7-alpine`
- [ ] 2.2 Add the four scenarios from the spec (TTL expiry timing, DeletePrefix namespace, PING failure fallback, container restart reconnect) and verify each `go test -run TestRedisCacheRealRedis/Scenario_<name> ./internal/platform/cache/...` passes with a live container
- [ ] 2.3 Verify `SKIP_CONTAINERS=true go test ./internal/platform/cache/...` calls `t.Skip` with the same wording pattern as the Postgres integration tests and exits 0
- [ ] 2.4 Verify `cd backend && make coverage-check` still passes with the new test contributing to the cache-package coverage (no drop below `COVERAGE_MIN`)

## 3. pip-audit wrapper + allowlist

- [ ] 3.1 Create `scripts/ci/pip_audit_with_allowlist.py` that runs `pip-audit --format=json`, applies a TOML allowlist (`[[advisories]]` with `id` / `date` / `rationale`), prints the matched rationale on a hit, treats entries with `date` older than 90 days as expired (exits 1 with a notice), and exits 2 when the allowlist file is missing or malformed; verify the script's unit tests pass via `python -m pytest scripts/tests -q` (the same harness `no-new-comments` already uses)
- [ ] 3.2 Create `backend/agent_py/.pip-audit-allowlist.toml` with a single comment-only header (`# One [[advisories]] block per deferred advisory; see scripts/ci/pip_audit_with_allowlist.py.`) and verify the wrapper exits 0 on an empty allowlist with no advisories present, exits 1 when a synthetic advisory is injected into the wrapper's test fixture

## 4. Workflow wiring

- [ ] 4.1 Add a `py-audit` job to `.github/workflows/ci.yml` (SHA-pinned `actions/checkout` / `actions/setup-python` per the pinning table at `ci.yml:9-16`, `dorny/paths-filter` with `filters: | agent-deps: ['backend/agent_py/**']`, `setup-python` cache keyed on `backend/agent_py/pyproject.toml`, `pip install -e .[ci]`, then `python scripts/ci/pip_audit_with_allowlist.py`); verify the file's YAML parses via `python -c "import yaml; yaml.safe_load(open('.github/workflows/ci.yml'))"`
- [ ] 4.2 Add `py-audit` to the `needs:` list of `e2e-test` (currently `[backend-test, frontend-test, govulncheck, npm-audit]`) so a failed `py-audit` blocks the e2e lane; verify the YAML still parses and the matrix ordering is preserved (no new circular `needs:`)
- [ ] 4.3 On first push, allowlist any pre-existing `pip-audit` advisories with a `## Rationale:`-style `rationale = "..."` string citing the upstream advisory URL, the upstream fix status, and the revisit trigger; verify `py-audit` goes green and the allowlist log line prints the rationale on match (parallel to the `govulncheck` precedent at `ci.yml:351-358`)
- [ ] 4.4 Add a sibling `e2e-test-redis` job to `.github/workflows/ci.yml`: copy the existing `e2e-test` shape, add a `services: redis: image: redis:7-alpine ports: ['6379:6379'] options: --health-cmd "redis-cli ping"` block (mirroring the existing postgres healthcheck pattern at `ci.yml:226-230`), add `REDIS_ENABLED: "true"` + `REDIS_URL: "redis://localhost:6379"` to the env, and verify the YAML parses via `python -c "import yaml; yaml.safe_load(open('.github/workflows/ci.yml'))"` and that the new job appears in the workflow's job list
- [ ] 4.5 Add `e2e-test-redis` to `release`'s `needs:` list (currently `[e2e-test, govulncheck, npm-audit]`); verify YAML still parses and `release` cannot run until both e2e lanes are green
- [ ] 4.6 Extend the backend `/api/health` readiness probe to include the cache mode in its response body (e.g. `services.cache: "noop"` or `"redis"` and `cache.ping: "ok"` / `"fail"` field), so the readiness loop in `ci.yml:285-299` (`grep -q '"database":"up"'`) can be mirrored as `grep -q '"cache":"redis"'` on the new lane; verify with `curl -fsS http://localhost:8080/api/health` after booting the backend with `REDIS_ENABLED=true` against a real Redis

## 5. Verification

- [ ] 5.1 Run `cd backend && SKIP_CONTAINERS=true go test ./...` locally and verify no regressions versus the baseline (suite count and pass count unchanged); record the before/after counts in the commit body
- [ ] 5.2 Run `cd backend && go test ./...` locally with Docker available and verify the new real-Redis suite runs end-to-end (TTL expiry in ~250 ms, DeletePrefix removes both `feeds:*` keys, PING-failure returns miss without panic, reconnect restores hit path); record wall-clock in the commit body
- [ ] 5.3 Bring up the dev stack locally with `make up` + `REDIS_ENABLED=true REDIS_URL=redis://localhost:6379 go run ./cmd/api` and verify `curl -fsS http://localhost:8080/api/health | jq .services.cache` returns `"redis"` and `cache.ping` returns `"ok"`; record the probe response in the commit body
- [ ] 5.4 Push the branch and verify the CI matrix on a green run: `backend-test` (with the real-Redis suite, coverage gate green), `py-audit` (green with rationale printed), `agent-alembic-check` (unchanged green), `govulncheck` (unchanged green), `npm-audit` (unchanged green), `frontend-test` (unchanged green), `e2e-test` (unchanged green, noop lane), `e2e-test-redis` (green, real-Redis lane); record the run URL in the commit body
