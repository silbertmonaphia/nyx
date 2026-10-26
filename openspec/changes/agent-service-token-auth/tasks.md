# Tasks

## 1. Go backend

- [x] 1.1 Add `AgentServiceToken` (`AGENT_SERVICE_TOKEN`) to `backend/internal/platform/config/config.go`: struct field, empty default, and an explicit `bindEnvVars()` allowlist entry (AutomaticEnv alone does not pick up new keys). `Load()` MUST NOT reject a missing or short token — the API stays up and the agent routes degrade at wiring time. Update `TestLoad_AgentEnabledTrueHonoured` to set a valid token. Verify config tests cover missing/short token loads successfully, valid token honoured, and agent-disabled optional (`go test ./internal/platform/config/...`).
- [x] 1.2 Stamp the service credential on every agent hop: add the `X-Nyx-Service-Token` constant and a `serviceToken` field to `AgentProxy` and `SessionsProxy`, set the header in `Run` and `Forward`, derive it in `newSessionsProxyFromService`, and reject an empty token in both constructors. Verify proxy tests assert the exact header on `/agent/run`, all five session routes, and approvals, and that `Authorization` remains unchanged (`go test ./internal/chat/...`).
- [x] 1.3 Wire `cmd/api/main.go`: when `AGENT_ENABLED=true` and the token is missing/short or `PYTHON_AGENT_URL` is empty, build a `NewServiceWithMisconfiguredAgent` so `/api/chat`, sessions, and approvals answer 503 with a static misconfiguration detail while the rest of the API runs (never a single-turn fallback); otherwise pass `cfg.AgentServiceToken` to `NewAgentProxy`. Verify `go build ./...` plus handler tests for the 503 chat/session/approval responses.
- [x] 1.4 Update the stale trust-model comments in `agent_proxy.go`, `sessions_proxy.go`, `approvals_proxy.go`, and `handler.go` in place (no new comment lines). Verify `python3 scripts/check_no_comments.py --staged` passes on the Go diff.
- [x] 1.5 Document the new variable in `backend/.env.example` (required to serve agent routes when `AGENT_ENABLED=true`; missing/short leaves the rest of the API up but every agent route 503; generated with `openssl rand -hex 32`, same value as the agent). Verify the commented placeholder and the degrade note are present.
- [x] 1.6 Run the Go suites: `cd backend && SKIP_CONTAINERS=true go test ./... -count=1`.

## 2. Python agent

- [x] 2.1 Add `populate_by_name=True`, the `service_token: SecretStr` field with `validation_alias="AGENT_SERVICE_TOKEN"`, and the ≥ 32-byte startup validator to `backend/agent_py/agent/config.py`. Verify new settings tests cover missing, short, valid, and unprefixed-alias precedence (`pytest tests/test_config.py`).
- [x] 2.2 Add `backend/agent_py/agent/service_auth.py` with `SERVICE_TOKEN_HEADER`, the constant-time byte comparison, and the exact `/healthz` exemption; register it in `create_app` after the rate limiter. Compare encoded bytes, not `str` (`hmac.compare_digest` raises `TypeError` on non-ASCII str). Verify `tests/test_service_auth.py` covers missing/wrong/valid token, a non-ASCII header value (401, never 500), health 200, metrics 401, and a valid token reaching a handler.
- [x] 2.3 Set a test token in `tests/conftest.py` before collection and add the header to every `create_app`-based request helper (run, chat sessions, GDPR, rate limit, BYOK, Langfuse, session continuation, heartbeat, chat titles, sweeper, prefs). Keep handler-level 400 assertions at 400; leave the bare-app approvals harness as handler-level coverage. Also give the eval suite a test token: `evals/tests/test_stub.py` constructs `Settings(...)` directly and the CI `Eval unit gate` step does not set one — pass `service_token=` explicitly or set the env in that suite/step. Verify `pytest tests/ -q` and `pytest evals/tests -q` pass.
- [x] 2.4 Update the agent trust documentation and docstrings in place: `README.md`, `README.zh.md`, `README_ALL.md`, `.env.example`, `docs_agent/AgentArchitecture.md`, and the route/rate-limit docstrings. Verify `grep -rn "trust boundary\|no auth\|network boundary" backend/agent_py` shows only the new service-token statements.
- [x] 2.5 Run `cd backend/agent_py && make lint` and the full agent suite with a test token.

## 3. Dev, eval, and e2e tooling

- [x] 3.1 Add `scripts/lib/agent-token.sh` (precedence: exported env → `backend/.env` assignment → persisted `backend/agent_py/.service-token` → generate `openssl rand -hex 32`, mode 600; reject < 32 bytes) and gitignore the persisted file. Verify two separate invocations resolve the same value and a short value is rejected.
- [x] 3.2 Wire the resolver into `scripts/dev.sh` (both backend and agent branches) and `scripts/e2e-side-stack.sh` (pass `AGENT_SERVICE_TOKEN` to both processes). Verify `make dev-backend`/`make dev-agent` agree and a side-stack boot reaches `/healthz` 200.
- [x] 3.3 Add `EVAL_AGENT_SERVICE_TOKEN` to `evals/providers/agent_run.py`: stamp the header on agent calls only, keep `auth_headers` for Go API calls, and fail preflight when the token is missing/short. Pass it through the `Makefile` eval target. Verify `pytest evals/tests -q` and a provider unit test asserting the header.
- [x] 3.4 Add `scripts/e2e/agent-service-auth.sh` plus a `make e2e-agent-auth` target: health without a header → 200; `/metrics` → 401; `/agent/run` missing/wrong token → 401 with the exact body; valid token without `X-Nyx-User-Id` → 400; `/agent/sessions` with token + user id → non-401. Verify against a running side stack without printing the token.
- [ ] 3.5 Update the stale compose trust comments and confirm the agent/backend `env_file: backend/.env` pairing already supplies `AGENT_SERVICE_TOKEN` (no new `environment:` entry). Verify with `docker compose --profile app config` that both services receive the variable. **Partially verified 2026-10-26**: comments updated and both services' `env_file: backend/.env` pairing confirmed by inspection; `docker compose --profile app config` could not run locally because `frontend/.env` is absent (compose aborts on the missing env file).

## 4. Operator documentation

- [x] 4.1 Add the public-Render service-token section to `docs/agent-deployment.md`: env row, generation, health-path precheck, Go-first deploy order, rotation, rollback, and the `X-Nyx-Service-Token` header on operator GDPR curls. Verify the commands and links are consistent with the shipped defaults.
- [x] 4.2 Update `docs/deployment.md`, `README.md`, `README.zh.md`, `README_FULL.md`, and `CLAUDE.md` so every agent deployment/config mention names `AGENT_SERVICE_TOKEN` and the Go-only caller model. Verify `grep -rn "PYTHON_AGENT_URL\|AGENT_ENABLED"` has no outdated sister surfaces.
- [x] 4.3 Rewrite the agent trust-boundary statements in `SECURITY.md` and `SECURITY.zh.md` (service-token gate, health exemption, constant-time compare, no credential logging) and verify they match the implemented status codes and routes.

## 5. Integration verification

- [x] 5.1 Add the narrow CI step to `agent-lint` running `pytest tests/test_service_auth.py tests/test_config.py -q` with a test token, and verify the workflow YAML parses.
- [x] 5.2 Run the full local gates: `cd backend && SKIP_CONTAINERS=true go test ./... -count=1`; agent `pytest tests/ -q`; `make lint`; `gofmt -l`; `pytest evals/tests -q`; `python3 scripts/check_no_comments.py --staged`.
- [x] 5.3 Bring up the side stack and run `scripts/e2e/agent-service-auth.sh`, then the live chat Playwright spec with `PLAYWRIGHT_BASE_URL=http://localhost:5273` and `E2E_API_URL=http://localhost:8180/api --workers=1`; record the result (state explicitly if vLLM/infra is unavailable). **Result 2026-10-26**: side agent :9100 + side backend :8180 (new code, shared infra DB/Redis) — `agent-service-auth.sh` passed all 8 checks; a real Go → Python `/api/chat` call produced `run_id` + terminal frames (token accepted) and `GET /api/chat/sessions` through Go returned 200; a wrong-token backend produced the fail-closed `{"detail":"unauthorized"}` with no agent run. The live Playwright chat spec did **not** run: no vLLM on :8000, so a real model answer was unavailable.
- [x] 5.4 Manually verify the mismatch case once: set a different token on Go, confirm chat fails closed with no agent run and no data leak, then restore.
- [x] 5.5 Verified the degraded mode against a real side backend (`AGENT_ENABLED=true`, `PYTHON_AGENT_URL` set, short `AGENT_SERVICE_TOKEN`): `/api/feeds` returned 200 with a valid JWT, while `POST /api/chat`, `GET /api/chat/sessions`, and `POST /api/chat/approvals` returned 503 with `details: "The agent service is misconfigured"`; the startup log named the missing token. The Go API stayed up; only the agent routes degraded.

## Workflow follow-up

- Archive this implementation change once all tasks above are complete and the review is approved.
- Rollout gates live in `openspec/changes/agent-service-token-auth-rollout/`; that change is PM/operator-owned and is not archived from this implementation session.
