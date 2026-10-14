# E2E evidence (task 7.2 + 7.1)

Live-stack smoke, 2026-10-14, branch `agent-feed-tools`.

**Stack:** worktree side stack (`scripts/e2e-side-stack.sh`, suffix `-ab`)
serving this branch's code — backend :8181 (`MCP_ENABLED=true`, JWT auth),
Python agent :9102 (migrations at head incl. 0006), SPA :5274
(`VITE_API_URL=:8181`), host vLLM :8000. The operator's stack
(:8080/:5173/:9000) was never touched.

**Live smoke (7.2)** — `frontend/tests/e2e/chat-feed-approval-live.spec.ts`:

```
PLAYWRIGHT_BASE_URL=http://localhost:5274 \
  npx playwright test tests/e2e/chat-feed-approval-live.spec.ts --timeout=240000
→ 1 passed (14.6s)
```

Observed flow, matching the shipped UI: fresh user registers → opens chat →
prompts the live planner to `create_feed` → the run **pauses on the inline
confirmation card** (tool `create_feed` + args + Approve/Deny, stream held)
→ Approve posts `{run_id, approval_id, decision}` to `/api/chat/approvals`
→ the resumed stream settles cleanly (`done`, no `error` frame) → chat closed,
page reloaded → **the created feed renders in `feed-list`** — the approved
call executed against the real Go MCP registry and Postgres, per-owner.

**Canned wire spec (7.1)** — `frontend/tests/e2e/chat-feed-approval.spec.ts`
on the same stack: **4 passed** (approve / deny / read-only-no-card /
stale-404→expired; the held-stream mock pauses one response and resumes it on
the decision POST).

Full-gate runs (7.4) and their environment notes are in `tasks.md` 7.4.
