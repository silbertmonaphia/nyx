# Design

## Context

Slices 1–2 store and surface chat history. This slice adds the memory layer: per-user key/value preferences extracted from conversation and injected into the planner prompt. See [`proposal.md`](./proposal.md) for motivation; the `agent-user-prefs` spec carries the behavioral contract; `AgentMemoryRoadmap.md` §Slice 3 locks the cross-cutting choices (async extraction, no mem0, no frontend changes).

Anchors in the current code: `planner._build_planner_messages` (line ~233) builds `SystemMessage(content=PLANNER_SYSTEM_PROMPT + planner_block(settings))` — the injection site; `main.py`'s `_stream_agent_run` finally-block records the assistant message (slice 1) — the extraction trigger point; `config.py` validator style is the settings template.

## Goals / Non-Goals

**Goals:**
- One small extraction call per preference-bearing turn, after `done`, never in the reply path
- Per-key upsert semantics (latest statement wins)
- Injection that is invisible when the user has no prefs (byte-identical prompt)
- Lifecycle: prefs die with their source session at retention time
- Kill switch (`PYTHON_AGENT_PREFS_ENABLED=false`) disabling extraction and injection together

**Non-Goals:**
- Cross-session semantic recall (top-K over sessions) — deferred by the roadmap
- mem0 / any external memory service
- Frontend surface for viewing/editing prefs (slice 4's export covers visibility)
- Graph/ontology-shaped memory — flat key/value only
- Preference confidence-weighted ranking beyond `updated_at DESC`

## Decisions

### D1. Flat key/value with per-key upsert, not an append-only log

A preference like "snack → 鱿鱼丝" has one current truth. `UNIQUE (user_id, key)` + upsert keeps exactly one row per key — the injection read is a simple `ORDER BY updated_at DESC LIMIT N`, and "what do we believe today" needs no dedup logic. An append-only log would need read-time reduction (group by key, take latest) on every planner call and grow unbounded. If provenance history is ever needed, `source_session_id` + `updated_at` in the audit path (slice 4) covers compliance; per-key history is not a requirement.

### D2. Extraction is fire-and-forget after the `done` path

The roadmap locked "after the `done` SSE frame, never inline". Implementation: in `_stream_agent_run`'s finally block, after `chat_store.record_message(assistant)` and `end_session`, spawn `asyncio.create_task(_extract_prefs_task(...))`. The task holds no reference to the response generator, so client disconnect cannot cancel the reply but may skip extraction — acceptable (the next preference-bearing turn re-extracts; keys are stable).

Alternative considered: a background worker queue (Redis stream / arq). Rejected: one small call per turn, best-effort semantics, no retry requirement — a task is the right size. If volume ever demands a queue, the extraction function is the seam.

### D3. The gate is a generous regex, shipped on

`looks_like_preference` matches first-person preference markers (EN + ZH, documented in the spec). It's cheap, deterministic, and testable. Trade-off: false negatives miss a preference (recoverable — the user will re-state or the phrasing will match next time); false positives cost one small LLM call. Biasing toward false negatives keeps cost bounded; the alternative (LLM-classifier gate) would double the call count for no user-visible gain.

### D4. The extraction call bypasses the per-user chat budget

The budget guard prices *user-visible* model work. Extraction is an internal byproduct, capped structurally (one call per preference-bearing turn, small prompt, small `max_tokens`). Charging it to the user would make prefs silently accelerate budget exhaustion — a UX trap. Result: extraction can run while a user is at their cap; chat calls still refuse with `budget_exceeded`. The cost watch gate lives in the rollout sibling (§3.2).

### D5. Injection appends at the end of the system prompt; empty set = byte parity

`prefs_block([])` returns `""`, so `PLANNER_SYSTEM_PROMPT + planner_block(settings) + prefs_block(prefs)` equals the current construction byte-for-byte when there are no prefs. This mirrors the web-tools parity contract (`planner.py` comment: "keeping the prompt byte-identical to its pre-injection form") and is pinned by a regression test. Ordering after `planner_block` keeps the static base first — providers cache the static prefix better.

### D6. Prefs die with their source session (hard-delete only)

`delete_prefs_for_sessions` runs in `RetentionCron._chat_iteration` right after the hard-delete statement, with the ids the hard-delete returned. Implementation: `DELETE FROM chat_user_prefs WHERE source_session_id = ANY($1::uuid[])` — plain exact-id matching, NO `USING chat_sessions` join. Safety is by construction: the sole caller passes ids from its own `DELETE ... RETURNING id`, and every returned id names a session whose prefs rows point at it — so the delete can only ever reach prefs of sessions that just died. **Amended 2026-10-14 (cross-agent conflict):** the original design prescribed the join; that was self-defeating (retention deletes the sessions first, so the join matches nothing) and the join never provided owner filtering anyway — only an existence check, which a foreign live session id would have passed.

Soft-delete (phase 1) deliberately leaves prefs injectable: a session inside the grace window is still "recently real", and an operator re-flipping the delete should see consistent behavior. The `NULL`-able `source_session_id` covers prefs extracted from turns whose session row is absent at write time (metadata-only dev) — a NULL-sourced pref follows the user-scoped erasure path (slice 4) instead.

### D7. Config surface: two vars

`PYTHON_AGENT_PREFS_ENABLED` (kill-switch, default true) and `PYTHON_AGENT_PREFS_MAX_INJECTED` (default 50, ≥ 0). No separate extraction-model var yet: the extraction call uses `settings.planner_model` (the cheap model by convention). If extraction cost shows in the rollout watch, a `PYTHON_AGENT_PREFS_MODEL` override is a one-line addition with no contract change.

## Risks / Trade-offs

- **[Injection leaks stale preferences]** A user who changed their mind ("我不再喜欢鱿鱼丝了") needs the new statement extracted; if the gate misses the negation phrasing, the stale value persists. → Mitigation: the gate regex includes negative-form markers ("不再喜欢"), and the extraction prompt instructs the model to emit corrected values for negations. Residual risk recorded for the rollout observation window; bounded by prefs being advisory context, not data.
- **[Extraction task outlives the run's event loop in tests]** A dangling `create_task` in pytest produces "task was destroyed but it is pending" noise and flakiness. → Mitigation: the task is tracked on `app.state` (a set with a done-callback discard), and tests await the tracked set before asserting; the pattern exists in the heartbeat task wiring.
- **[Prompt growth]** 50 prefs × ~40 chars ≈ 2 KB added to every planner call. → Mitigation: `PREFS_MAX_INJECTED` cap; the cost watch gate measures real token deltas; 50 is generous for the key/value regime.
- **[Cross-user leakage via `source_session_id` matching]** The retention caller owns the ids it passes (they come from its own `DELETE ... RETURNING id`), so exact-id matching cannot reach another user's rows — a prefs row's `source_session_id` points at the session that produced it, and only those sessions' rows are in the list. → Mitigation: exact-id deletion per the amended D6, no join; the cross-user survival test in `test_retention_chat.py` pins it.
- **[Gate false positives spend calls on adversarial input]** A user spamming "我喜欢X" inflates extraction calls. → Mitigation: structurally bounded to one call per turn, same as any chat turn's budget; the rollout cost watch would surface abuse; the budget guard still caps their chat calls.

## Migration Plan

Alembic `0005_chat_user_prefs` (down_revision `0004_chat_sessions_and_messages`). New table only — no backfill (no historical prefs exist to derive; retro-extraction over stored sessions is explicitly out of scope — it would need a replay tool and a consent story). Rollback: `alembic downgrade -1` drops the table; extraction/injection code is gated by `PYTHON_AGENT_PREFS_ENABLED=false` for an operational rollback without a code change.
