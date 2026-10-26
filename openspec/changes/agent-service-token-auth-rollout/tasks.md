# Tasks

> Rollout gates split out of `agent-service-token-auth` per the rollout-split convention (`openspec/config.yaml`). The implementation change is code-complete and review-approved by the time this change is reached; these gates need the deployed Render services. PM-owned: do not archive from a dev session. Deploy-doc reference: `docs/agent-deployment.md`.

## 1. Configuration

- [ ] 1.1 Generate the token once with `openssl rand -hex 32` and set `AGENT_SERVICE_TOKEN` to that same value on both Render services (`nyx-api` and the dashboard-created agent web service). Verify the value is stored as a secret env var on each service and is never committed; record only "set on both" in the evidence log.
- [ ] 1.2 Confirm the agent service's Render health-check path is exactly `/healthz`. Verify the dashboard setting before deploying the enforcing Python image; a `/` path would receive 401 and the deploy would never become healthy.

## 2. Deploy and verify

- [ ] 2.1 Deploy the Go backend first, while the old Python image still ignores the header. Verify the SPA login, feed, and chat still work against the old Python image, and that the Go-to-agent requests carry the header (no token value in logs).
- [ ] 2.2 Deploy the enforcing Python image. Verify directly against the agent URL: no token or a wrong token on `/agent/run`, `/agent/sessions*`, `/agent/me/*`, and `/agent/approvals` returns 401 `{"detail":"unauthorized"}`; `/healthz` returns 200; `/metrics` on the application port returns 401; and a chat, session list, and approval flow through the SPA/Go still succeed.
- [ ] 2.3 Record the verification evidence in this file (status codes, timestamps, service image tags). Do not record token values.

## 3. Rotation and rollback

- [ ] 3.1 Rotation drill: set the new token on the Go service first, then on the agent service. Verify chat works after both sides converge; record the expected brief rejection window if the order is reversed.
- [ ] 3.2 Rollback drill: redeploy the previous Python image (which ignores the header) first, then revert the Go change or restore the previous token value on both services. Verify chat works; record the outcome.

## 4. Archive

- [ ] 4.1 After §1–§3 are checked: archive this change (PM/operator only — the implementer leaves this unchecked). Verify `openspec validate --archived` passes post-archive.

## References

- `openspec/changes/agent-service-token-auth/` — implementation change (proposal, design, specs, tasks).
- `openspec/specs/chat-agent-proxy/spec.md` + `openspec/specs/agent-python-service/spec.md` — behaviour specs synced at archive.
- `docs/agent-deployment.md` — env setup, deploy order, verification curls, rotation, rollback.
