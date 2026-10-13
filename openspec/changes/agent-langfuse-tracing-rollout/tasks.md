# Tasks

> Consolidated production-rollout gates for `agent-langfuse-tracing`. The implementation change is
> code-complete in dev/CI (including its committed Playwright e2e) and archives independently;
> this change tracks the gates that need a deployed stack, rotated production secrets, or an
> observation window. PM/operator-owned — the implementer runs the gates, records evidence, and
> leaves the archive task unchecked.

## 1. Production stack deploy

- [ ] 1.1 Provision the `langfuse` profile services on the deploy host with **production**
  secrets — rotate every dev default from the root `.env.example` (`SALT`, `ENCRYPTION_KEY`,
  `NEXTAUTH_SECRET`, Postgres/ClickHouse/Redis/MinIO passwords, `LANGFUSE_INIT_*` keys) and
  keep the loopback-only single-published-port posture from the implementation change (design
  D1). Verify: UI reachable from the operator network; the agent's configured key pair
  authenticates (`GET /api/public/traces` → 200); no stateful component port reachable from
  outside the host. Cites: `backend/agent_py/README.md` (bring-up), root `.env.example`.

- [ ] 1.2 Retention + backup policy for trace data. Trace payloads carry user-derived content:
  configure instance retention aligned with the run-retention parity goal
  (`PYTHON_AGENT_RETENTION_DAYS` = 30d), backups for `langfuse-db` / ClickHouse / MinIO volumes,
  and run a restore drill on a non-production host. Verify: policy documented; restore drill
  recorded with pass/fail.

- [ ] 1.3 Deploy notes update — add the Langfuse section (rollout order, secret management,
  sizing) to the deploy documentation set. Depends on the production deploy; record the commit
  once landed.

## 2. Production gates

- [ ] 2.1 Suites green ≥ 1 week — backend + agent pytest suites (including the new tracing
  tests) green in CI for at least one week before any production flip. Wait-and-record.

- [ ] 2.2 Staging flip — set `PYTHON_AGENT_LANGFUSE_ENABLED=true` + staging host/keys in
  staging. Run chats covering an `llm`-step plan and a `tool`-step plan; verify in the staging
  Langfuse UI: one session per `run_id`, correct user ids, planner + executor generations with
  provider usage, tool spans with durations. Verify the same runs' Jaeger traces are unchanged
  (the D2 provider-isolation contract holds in a deployed stack).

- [ ] 2.3 Canary observation window — watch ingestion volume, ClickHouse/MinIO disk growth, and
  agent latency delta against the tracing-disabled baseline; confirm within the capacity budget
  before the production flip.

- [ ] 2.4 Rollback drill in staging — (a) flip `PYTHON_AGENT_LANGFUSE_ENABLED=false`: runs
  unaffected, no egress to the Langfuse stack; (b) with the flag on, stop the Langfuse stack:
  runs still reach `done` + `terminator` with identical SSE; export failures surfaced in the
  logs (the agent's own open/update/close paths log at warn; the OTLP processor logs its failed
  batches at error — both are the expected signal, neither affects the run). Re-up restores
  trace flow with no agent restart required. Record outcomes.

- [ ] 2.5 Production flip — after 2.1–2.4, set `PYTHON_AGENT_LANGFUSE_ENABLED=true` + production
  host/keys. Record the flip in the deploy notes.

- [ ] 2.6 Archive this change once 2.1–2.5 are checked. PM only.

## References

- `openspec/changes/agent-langfuse-tracing/` — implementation change (design D1/D2/D6 define the
  stack shape, provider isolation, and masking contract).
- `backend/agent_py/README.md` — bring-up commands + env reference for the Langfuse keys.
- Root `.env.example` — dev defaults that MUST be rotated for any deployed environment.
