# E2E evidence — agent-user-preferences (2026-10-14)

Environment: side stack on spare ports (`scripts/e2e-side-stack.sh`: Go :8180, Python agent :9100 with the slice-3 code, Vite :5273) + shared infra (Postgres :5433, Redis :6380) + host vLLM (:8000). Operator stack untouched.

**Full Playwright suite, serial (CI mode): 27 passed (2.7m).** Includes `chat-user-prefs.spec.ts` — the live leg `a stated preference survives into a later session and the turns replay` (38.2s; extraction → `+ New chat` → ask → replay assertions → **plus the review-M2 deterministic plumbing assert**: a polled DB query through the agent venv's python + asyncpg confirming a `chat_user_prefs` row sourced from the first session id) — and the full pre-existing chat suite (sidebar 6/6, chat-rag 7/7, replays, langfuse, feeds): no cross-spec regressions from the planner-prompt threading (empty-prefs parity holds live).

**Backend suite (nyx_test): 410 passed, 1 skipped** — includes the review follow-through tests: orphan-sweep backstop (stranded/live/NULL rows), extraction bounds (key charset, value cap, per-turn cap, block flattening), differential budget-spy (extraction adds zero budget consultations), injection-load-failure degradation.

Database-level confirmation from the live runs (shared `nyx` DB):
```
SELECT user_id, key, value, source_session_id IS NOT NULL FROM agent.chat_user_prefs;
646 | music_genre | "ACG"    | src: true
646 | snack       | "鱿鱼丝" | src: true
621 | music_genre | "ACG"    | src: true
621 | snack       | "鱿鱼丝" | src: true
```
Two distinct e2e users each got both keys extracted from "我喜欢鱿鱼丝，也喜欢ACG", each anchored to its source session — the async extraction pipeline runs after `done` without delaying the stream (the spec's stream-settle assistor is the evidence: adds ~seconds after the reply, chat itself unaffected).

Failure-isolation and gate coverage live at unit level (tests/test_prefs_injection.py: stub-raises leg, gate-miss leg with `call_count == 0`, kill-switch leg; tests/test_prefs.py: malformed-output leg); the parity contract is byte-pinned in unit tests.
