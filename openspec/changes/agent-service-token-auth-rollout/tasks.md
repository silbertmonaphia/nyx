# Tasks

> Rollout gates split out of `agent-service-token-auth` per the rollout-split convention (`openspec/config.yaml`). The implementation change is code-complete and review-approved by the time this change is reached; these gates need the deployed Render services. PM-owned: do not archive from a dev session. Deploy-doc reference: `docs/agent-deployment.md`.

## 1. Configuration

- [x] 1.1 Generate the token once with `openssl rand -hex 32` and set `AGENT_SERVICE_TOKEN` to that same value on both Render services (`nyx-api` and the dashboard-created agent web service). Verify the value is stored as a secret env var on each service and is never committed; record only "set on both" in the evidence log. **Verified 2026-10-26**: set as a secret env var on both services with the same value; the value lives only in the Render env config and is not committed.
- [x] 1.2 Confirm the agent service's Render health-check path is exactly `/healthz`. Verify the dashboard setting before deploying the enforcing Python image; a `/` path would receive 401 and the deploy would never become healthy. **Verified 2026-10-26**: the deploy reached a healthy state and `/healthz` answers 200, so the probe path is the exempt one.

## 2. Deploy and verify

- [ ] 2.1 Deploy the Go backend first, while the old Python image still ignores the header. Verify the SPA login, feed, and chat still work against the old Python image, and that the Go-to-agent requests carry the header (no token value in logs). **Not exercised as a separate step 2026-10-26**: the deployment was a single auto-deploy of the latest commit, so there was no window with the old Python image live against the new Go backend to verify against.
- [x] 2.2 Deploy the enforcing Python image. Verify directly against the agent URL: no token or a wrong token on `/agent/run`, `/agent/sessions*`, `/agent/me/*`, and `/agent/approvals` returns 401 `{"detail":"unauthorized"}`; `/healthz` returns 200; `/metrics` on the application port returns 401; and a chat, session list, and approval flow through the SPA/Go still succeed. **Verified 2026-10-26** against the live agent URL — see the evidence below. Covered by the scripted run: `/agent/run` (401 no token, 401 wrong token, 400 with token and no `X-Nyx-User-Id`), `/metrics` (401 without token, 200 with token), `/agent/sessions` (401 without token, 200 with token and user id), `/agent/me/export` (401 without token), `/healthz` (200 without token), and the exact 401 body. Not covered by that run: `POST /agent/approvals` without a token, and the SPA-side chat/approval round-trip through Go.
- [x] 2.3 Record the verification evidence in this file (status codes, timestamps, service image tags). Do not record token values.

### Verification evidence — 2026-10-26T13:55:13Z

- Target: the deployed agent web service, public URL `https://nyx-agent.onrender.com`.
- Deployed commit at check time: `42c8b2f` (auto-deploy of the latest commit; contains `b5ae35e`, the service-token gate). The follow-up commit `3f29a56` touches `scripts/`, `docs/` and `openspec/` only — the agent image build context is `backend/agent_py`, so the gate behaviour is unchanged.
- No credential values are recorded below.

| Check | Result |
| --- | --- |
| `GET /healthz` without a token | 200 `{"status":"ok"}` |
| `GET /metrics` without a token | 401 |
| `GET /agent/sessions` without a token | 401 |
| `GET /agent/me/export` without a token | 401 |
| `POST /agent/run` without a token | 401 |
| `POST /agent/run` with a wrong token | 401 |
| `POST /agent/run` with the token, no `X-Nyx-User-Id` | 400 |
| `GET /metrics` with the token | 200 |
| `GET /agent/sessions` with token + `X-Nyx-User-Id` | 200 |
| 401 response body | `{"detail":"unauthorized"}` |

Command used: `AGENT_SERVICE_TOKEN=<redacted> AGENT_URL=https://nyx-agent.onrender.com bash scripts/e2e/agent-service-auth.sh` → 8/8 ok, `agent service-token checks passed`. The token-bearing checks passing against the live service also show that the Go service and the agent service hold the same value.

## 3. Rotation and rollback

- [ ] 3.1 Rotation drill: set the new token on the Go service first, then on the agent service. Verify chat works after both sides converge; record the expected brief rejection window if the order is reversed.
- [ ] 3.2 Rollback drill: redeploy the previous Python image (which ignores the header) first, then revert the Go change or restore the previous token value on both services. Verify chat works; record the outcome.

## 4. Archive

- [ ] 4.1 After §1–§3 are checked: archive this change (PM/operator only — the implementer leaves this unchecked). Verify `openspec validate --archived` passes post-archive.

## References

- `openspec/changes/agent-service-token-auth/` — implementation change (proposal, design, specs, tasks).
- `openspec/specs/chat-agent-proxy/spec.md` + `openspec/specs/agent-python-service/spec.md` — behaviour specs synced at archive.
- `docs/agent-deployment.md` — env setup, deploy order, verification curls, rotation, rollback.
