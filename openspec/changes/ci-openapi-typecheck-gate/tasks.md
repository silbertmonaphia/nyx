# Tasks

## 1. CI wiring

- [x] 1.1 Add a `Frontend OpenAPI type drift check` step to the `frontend-test` job in `.github/workflows/ci.yml` running `cd frontend && npm run openapi:check`, placed with the other static checks (after `Architecture boundary check`, before `Test`) so drift fails in seconds rather than after the build; verify the file's YAML parses via `python3 -c "import yaml; yaml.safe_load(open('.github/workflows/ci.yml'))"` (2026-10-15: step present in the parsed step list between `Architecture boundary check` and `Test`; YAML loads clean)
- [x] 1.2 Verify the workflow delta is exactly one step: `git diff .github/workflows/ci.yml` adds no `uses:` line, and `frontend/package.json` / `frontend/package-lock.json` are untouched (2026-10-15: `+13` lines, `grep -c "^[-+] *uses:"` = 0, `git status frontend/` clean)

## 2. Verification

- [x] 2.1 Run `cd frontend && npm run openapi:check` on the clean tree and confirm exit 0 (2026-10-15: exit 0)
- [x] 2.2 Negative test: plant drift in `frontend/src/api/openapi/schema.d.ts` (drop a field the spec declares), confirm the check exits non-zero and prints the differing lines, then restore the file and confirm the check is green again — the gate must fail closed, not silently regenerate (2026-10-15: renamed `cache_ping` → `cache_ping_drifted`, ran the step's exact shell block → `step_exit=1` with the `diff -u` output; restored from a byte copy → check exit 0, `git status frontend/` clean)
- [x] 2.3 Confirm the failure output names the stale file and `npm run openapi:gen`, so the fix is discoverable from the job log alone (the spec's "remedy is discoverable" scenario); if the script's output does not name the command, extend the step to echo it — but do not change `frontend/package.json`'s script contract (2026-10-15: `npm run openapi:check` alone prints only the diff, so the step carries the `::error::` annotation plus `Fix: cd frontend && npm run openapi:gen …`; `package.json` untouched)
- [ ] 2.4 Push and verify `frontend-test` runs the new step and is green on the pushed commit; record the run URL

## 3. Docs

- [x] 3.1 Name the frontend half of the drift contract where the backend half is documented: `backend/HUMA.md` (regen block gains the two `npm run openapi:*` commands; the `## CI` paragraph states the `frontend-test` gate) and `Harness.zh.md`'s quality-gate inventory ("OpenAPI drift（后端 `make openapi-diff`，前端 `npm run openapi:check`）")
