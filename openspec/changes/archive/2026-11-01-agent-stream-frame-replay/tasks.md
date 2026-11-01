# Tasks

## 1. Python agent

- [x] 1.1 `agent/frames.py` (new): `FrameLog` — an in-process ring (`collections.deque(maxlen=N)`) holding `(seq, frame_bytes)`, with `append(bytes) -> seq`, `since(cursor) -> list[bytes] | TRUNCATED`, `watermark() -> seq`, and `oldest_seq()`. Cursor parsing accepts a decimal id and reports malformed input as truncation. Pure unit tests (no Redis, no DB): append/trim, cursor hit, cursor at the oldest retained frame, cursor rolled past, malformed cursor.
- [x] 1.2 Session wiring: `_RunSession` owns a `FrameLog`; the single emit helper encodes → assigns seq → appends → broadcasts `(seq, bytes)`; `id: <seq>\n` (plain decimal) is prepended to the frame. `finalize()` drops the log with the session. pytest asserts ids are present and strictly increasing, the log is empty after terminal, and no network call is made on the emit path (the buffer is in-process).
- [x] 1.3 Re-attach cursor branch: read `Last-Event-ID`, then `last_event_id`; with a cursor take the watermark before subscribing, replay `since(cursor)`, then bridge the subscriber queue filtered to `seq > watermark`; without a cursor keep today's step projection; truncation (rolled past, malformed, or replay disabled) emits `note{kind:"replay_truncated"}` before the projection. pytest per scenario, including "no duplicate and no gap" across a synthetic emit-during-replay.
- [x] 1.4 Config: `PYTHON_AGENT_FRAME_REPLAY_ENABLED` (bool, default true), `PYTHON_AGENT_FRAME_BUFFER_MAX` (int ≥ 1, default 5000); startup validation names the offending key; pytest covers defaults and invalid values. Run the suite once without `TEST_ASYNC_PG_DSN`.
- [x] 1.5 End-to-end (DSN-gated): a run whose client disconnects mid-answer, with frames emitted while detached (pushed onto the run's own sink queue, so they travel the real drain → encode → buffer → broadcast path), re-attaches from the last seen id and delivers every missed frame in order before the live ones — asserted by the id range the response carries (every id in `(cursor, watermark]`) and by the merged delta text containing the full generated answer. The assertion is mutation-checked: ignoring the cursor in the endpoint makes it fail.

## 2. Go proxy

- [x] 2.1 `runs_proxy.go`: forward the caller's `Last-Event-ID` header verbatim and preserve `last_event_id` on the outbound query; nothing else changes (response stays byte-for-byte). No new route, no new limiter class.
- [x] 2.2 Handler tests (httptest): header forwarded; query preserved; both absent → neither sent; an `id:`-bearing SSE fixture survives verbatim through `copyAndFlush` (the existing keep-alive fixture test gains an id line).

## 3. SPA

- [x] 3.1 `chatService`: parse `id:` into the yielded event (extend the shared parser, no second parser); thread the cursor onto `reattachRun`'s request as `Last-Event-ID`. Vitest over byte fixtures including an id-less stream (cursor untouched) and a replayed stream (ids preserved).
- [x] 3.2 `chatStore`: retain the highest rendered id per run; drop frames at or below it; present the cursor on the ladder's re-attach rung; on `note{kind:"replay_truncated"}`, keep the partial row, set a "possibly incomplete" marker on the turn, and continue the ladder. Vitest per spec scenario, including append-without-gap and duplicate suppression at the replay/live boundary.
- [x] 3.3 Types + i18n: `replay_truncated` note kind and the "possibly incomplete" turn state, strings in every existing locale (en + zh).

## 4. Cross-runtime contracts + e2e

- [x] 4.1 Wire fixture: the Python emitter produces an `id:`-bearing stream (delta + done + `[DONE]`), pinned byte-for-byte in `test_frame_id_wire.py` and consumed by `chatService.test.ts` through the real parser — producer bytes ↔ consumer parser, per the project rule.
- [x] 4.2 Playwright `chat-stream-frame-replay.spec.ts` (agent-free, route-intercepted): cut mid-answer, re-attach serves the missed frames from the cursor, and the rendered answer equals the uninterrupted answer character-for-character (the gap-free assertion); plus a truncated-cursor case showing the marker. Runs serially (`--workers=1`) against the dev stack.

## 5. Gates + docs

- [x] 5.1 Gate sweep: `backend/agent_py` pytest bare + with `TEST_ASYNC_PG_DSN`/`PYTHON_AGENT_DATABASE_URL` on `nyx_test`; `backend` build + `SKIP_CONTAINERS=true go test ./...` + golangci-lint; `frontend` lint + test + typecheck + build; ruff + `scripts/check_no_comments.py`; no sqlc surface touched.
- [x] 5.2 Docs: the two env keys into `backend/agent_py/.env.example` + `README.md` / `README.zh.md` / `README_ALL.md` env tables; `FUTURE_AGENT.md` and `FUTURE_FRONTEND.md` updated (the frame-replay item is the one this change ticks). No Redis dependency is introduced, so the cache-posture docs are unchanged.
- [x] 5.3 Self-review pass over the full diff vs `mvp`: single emit path (no duplicated streaming loop), the append-before-broadcast invariant, the watermark boundary, a rolled-past cursor never claims replayability, no `err.Error()` on the wire, no comments on new lines.
