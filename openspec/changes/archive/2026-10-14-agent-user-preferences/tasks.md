# Tasks

## 1. Schema

- [x] 1.1 Add alembic `0005_chat_user_prefs.py` (down_revision `0004_chat_sessions_and_messages`): table `agent.chat_user_prefs(id bigserial PK, user_id bigint NOT NULL, key text NOT NULL, value jsonb NOT NULL, source_session_id uuid NULL, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now())`; UNIQUE index `(user_id, key)`; read index `(user_id, updated_at DESC)`. Verify: `alembic upgrade head` succeeds on the local Docker Postgres; `alembic downgrade -1` drops cleanly; `alembic upgrade head` again.

## 2. Settings

- [x] 2.1 Add `prefs_enabled: bool = True` and `prefs_max_injected: int = 50` to `backend/agent_py/agent/config.py` + `_validate_prefs` validator (raises when `prefs_max_injected < 0`) plus the default-values scenario updates. Verify: defaults load; `PYTHON_AGENT_PREFS_MAX_INJECTED=-1` raises; `PYTHON_AGENT_PREFS_ENABLED=false` loads.

## 3. PrefsStore

- [x] 3.1 Add `backend/agent_py/agent/prefs.py` with `Pref` dataclass (key, value, confidence=1.0, source_session_id, updated_at) and `PrefsStore` (same pool + `_with_retry` posture as ChatStore): `upsert_pref(user_id, key, value, source_session_id) -> None` (INSERT ... ON CONFLICT (user_id, key) DO UPDATE SET value, source_session_id, updated_at=now()); `load_prefs(user_id, *, limit) -> list[Pref]` (ORDER BY updated_at DESC LIMIT); `delete_prefs_for_user(user_id) -> int`; `delete_prefs_for_sessions(session_ids: list[str]) -> int` implemented as exact-id deletion per the amended design D6 (`DELETE ... WHERE source_session_id = ANY($1)`, no join — the original join form was self-defeating; see the 2026-10-14 cross-agent ruling), plus `delete_orphaned() -> int` (the H1 backstop sweep). Verify: `python -m pytest tests/test_prefs.py -v` covers upsert-in-place (one row per key, value/updated_at advance), cross-user isolation, limit ordering, delete-by-session with a foreign-session id present (must not touch another user's rows).

## 4. Extraction pipeline

- [x] 4.1 Implement in `prefs.py`: `looks_like_preference(text) -> bool` (documented EN+ZH first-person regex, comment the patterns), `extraction_prompt(messages) -> str` (JSON-output instruction + few-shot with the negation rule from design D5), `extract_preferences(messages, *, settings, client) -> list[Pref]` (one `ChatOpenAI` call on `settings.planner_model`, parse JSON array, Pydantic-validate; malformed → warn + `[]`), and `prefs_block(prefs) -> str` (empty list → `""`; else a compact deterministic block, sorted by key for stability). Verify: `python -m pytest tests/test_prefs.py -v` covers gate hits/misses (EN+ZH+casual), parse of a canned response via a stub client (monkeypatched), malformed JSON → `[]` + warn, `prefs_block([]) == ""` and stable ordering.

## 5. Async wiring

- [x] 5.1 In `main.py` `_stream_agent_run`'s finally block, after the assistant record + end_session: when `settings.prefs_enabled` and the gate passes on the last user message, spawn a tracked task (`app.state.prefs_tasks` set + done-callback discard, mirroring the heartbeat-task hygiene) that runs extract → upsert. Extraction failure: warn-log only. The extraction call MUST NOT consult the budget guard. Verify: `python -m pytest tests/test_prefs.py -v` — a run with a preference message writes a prefs row (await the tracked tasks); a run whose extraction client raises still reaches `terminator` and logs a warning; `prefs_enabled=false` → no task spawned.

## 6. Planner injection

- [x] 6.1 In `planner.py` `_build_planner_messages`, load prefs via the request's PrefsStore (thread the store through the planner call path — follow how other per-request deps reach the planner) and append `prefs_block(prefs)` to the system prompt content after `planner_block(settings)`. `prefs_enabled=false` or empty prefs → byte-identical prompt (pin with a test comparing against the pre-injection construction). Verify: `python -m pytest tests/test_prefs.py -v` covers the injection present/absent + parity bytes; full `python -m pytest tests/ -q` green.

## 7. Retention coupling

- [x] 7.1 In `retention.py` `_chat_iteration`, after `hard_delete_soft_deleted`: capture the hard-deleted session ids (extend `hard_delete_soft_deleted` to RETURN the ids or add a sibling method — keep the counter semantics: count = len(ids)) and call `delete_prefs_for_sessions(ids)`; failures warn-log without aborting. Verify: `python -m pytest tests/test_retention_chat.py -v` — a hard-deleted session's prefs vanish in the same iteration; another session's prefs survive.

## 8. Docs

- [x] 8.1 Update `backend/agent_py/.env.example` (prefs block: both vars, the gate note, kill-switch), `backend/agent_py/README.md` (env table row), `FUTURE_AGENT.md` §2.1 (tick the preference-memory bullet, reference the archived change), `AgentMemoryRoadmap.md` Slice 3 tick after archive. Verify: `git diff --check`; README env-var count matches code.

## 9. Self-review

- [x] 9.1 Spawn the `test-reviewer` agent against the Slice 3 diff; fix every CRITICAL/HIGH before archive; file the report as `review.md` in the change folder.

## 10. E2E

- [x] 10.1 Full stack up (host vLLM). Manual live smoke (record as `e2e-evidence.md`): (a) send "我喜欢鱿鱼丝，也喜欢ACG" → verify `agent.chat_user_prefs` rows appear within seconds; (b) new session → ask "推荐一个零食" → record whether the answer leans on the pref (model nondeterminism tolerated) AND verify via log/trace that the planner prompt carried the block; (c) send "今天天气怎么样" → no extraction call (gate); (d) `PYTHON_AGENT_PREFS_ENABLED=false` side instance → byte-identical prompt check. Write `frontend/tests/e2e/chat-user-prefs.spec.ts` covering the UI-visible legs (pref message → new session → ask; assertions tolerant of model phrasing, stub-replay for the deterministic parts per the `chat-agent-planner.spec.ts` pattern). If the live stack is unavailable, unit coverage in §3–§7 is the accepted substitute with that rationale recorded.

## 12. Review follow-through (2026-10-14)

- [x] 12.1 H1 — `PrefsStore.delete_orphaned()` per-tick backstop sweep + `agent_chat_prefs_deleted_total` counter; spec scenario added; pinned by `test_orphan_sweep_collects_stranded_prefs`.
- [x] 12.2 M1 — extraction bounds (key regex, 512-char value cap, 20/turn cap) + `max_tokens`/`timeout` on the extraction client + `prefs_block` newline flattening; four new tests.
- [x] 12.3 M2 — e2e spec now asserts the plumbing: `countPrefsForSession` DB poll (venv python + asyncpg, no new deps) anchored to the first session id.
- [x] 12.4 M3 — `.env.example` + `README_ALL.md` env surface; `FUTURE_AGENT.md` §2.1 annotated; roadmap kill-switch name corrected.
- [x] 12.5 M4/L1/L5 — design risk bullet + spec rationale reworded to the no-join form; task/proposal drift fixed; rollout doc var-name fixed.
- [x] 12.6 L3 — ruff clean on all touched files; L2 — debug script excluded from commits (operator cleanup noted).

## 11. Archive

- [x] 11.1 Run `openspec archive agent-user-preferences --yes`; verify `openspec validate --archived` passes; change moves to `openspec/changes/archive/2026-10-14-agent-user-preferences/`.
