# Tasks

## 1. Python agent

- [ ] 1.1 `agent/frames.py` (new): `FrameBuffer` over the existing `app.state.redis` client — `append(run_id, frame_bytes) -> entry_id` (`XADD agent:frames:{run_id} MAXLEN ~ N` + `EXPIRE`), `since(run_id, cursor) -> list[bytes]` (`XRANGE (cursor +`), `tail(run_id) -> entry_id | None` (`XREVRANGE … COUNT 1`), `oldest(run_id) -> entry_id | None`, `clear(run_id)`. Unit tests with a fake Redis assert trimming, expiry, and the cursor semantics including an unknown/expired cursor.
- [ ] 1.2 Emit path: every frame on `/agent/run` and on re-attach goes through one helper that (when enabled) `XADD`s the finished bytes, prepends `id: <entry>\n`, then broadcasts `(entry_id, bytes)`. Wire the flag and the degradation branch (write failure → emit without id, log warn, continue). pytest asserts ids are present, monotonic, and that a simulated Redis failure leaves the run healthy and the frames id-less.
- [ ] 1.3 Re-attach cursor branch: read `Last-Event-ID` header, then `last_event_id` query; with a cursor, take the watermark (`tail`) before subscribing, replay `since(cursor)`, then bridge the subscriber queue filtered to `entry_id > watermark`; without a cursor keep today's step projection. Truncation (cursor not in buffer / malformed) emits `note{kind:"replay_truncated"}` before the projection. pytest per scenario, including "no duplicate and no gap" across a synthetic emit-during-replay.
- [ ] 1.4 Config: `PYTHON_AGENT_FRAME_REPLAY_ENABLED` (bool, default true), `PYTHON_AGENT_FRAME_BUFFER_MAX` (int ≥ 1, default 5000), `PYTHON_AGENT_FRAME_BUFFER_TTL_SECONDS` (int ≥ 1, default 3600, must exceed `detached_run_cap_seconds`); startup validation names the offending key; pytest covers defaults, invalid values, and the TTL-vs-cap rule. Run the suite once without `TEST_ASYNC_PG_DSN`.
- [ ] 1.5 Terminal cleanup: `finalize()` clears the run's buffer; a cursor presented after the clear reports truncation. pytest asserts the key is gone at terminal and that an old cursor does not resurrect a finished run's frames.
- [ ] 1.6 DSN-gated end-to-end: a run whose client disconnects mid-answer, with frames emitted while detached, re-attaches from the last seen id and delivers every missed frame in order before the live ones (exit condition asserted on the merged text equalling the single-shot answer).

## 2. Go proxy

- [ ] 2.1 `runs_proxy.go`: forward the caller's `Last-Event-ID` header verbatim and preserve `last_event_id` on the outbound query; nothing else changes (response stays byte-for-byte). No new route, no new limiter class.
- [ ] 2.2 Handler tests (httptest): header forwarded; query preserved; both absent → neither sent; a `id:`-bearing SSE fixture survives verbatim through `copyAndFlush` (the existing keep-alive fixture test gains an id line).

## 3. SPA

- [ ] 3.1 `chatService`: parse `id:` into the yielded event (extend the shared parser, no second parser); thread the cursor onto `reattachRun`'s request as `Last-Event-ID`. Vitest over byte fixtures including an id-less stream (cursor untouched) and a replayed stream (ids preserved).
- [ ] 3.2 `chatStore`: retain the highest rendered id per run; drop frames at or below it; present the cursor on the ladder's re-attach rung; on `note{kind:"replay_truncated"}`, keep the partial row, set a "possibly incomplete" marker on the turn, and continue the ladder. Vitest per spec scenario, including append-without-gap and duplicate suppression at the replay/live boundary.
- [ ] 3.3 Types + i18n: `replay_truncated` note kind and the "possibly incomplete" turn state, strings in every existing locale (en + zh).

## 4. Cross-runtime contracts + e2e

- [ ] 4.1 Wire fixture: the Python emitter produces an `id:`-bearing stream (delta + done + `[DONE]`), pinned byte-for-byte in `test_frame_id_wire.py` and consumed by `chatService.test.ts` through the real parser — producer bytes ↔ consumer parser, per the project rule.
- [ ] 4.2 Playwright `chat-stream-frame-replay.spec.ts` (agent-free, route-intercepted): cut mid-answer, re-attach serves the missed frames from the cursor, and the rendered answer equals the uninterrupted answer character-for-character (the gap-free assertion); plus a truncated-cursor case showing the marker. Runs serially (`--workers=1`) against the dev stack.

## 5. Gates + docs

- [ ] 5.1 Gate sweep: `backend/agent_py` pytest bare + with `TEST_ASYNC_PG_DSN`/`PYTHON_AGENT_DATABASE_URL` on `nyx_test`; `backend` build + `SKIP_CONTAINERS=true go test ./...` + golangci-lint; `frontend` lint + test + typecheck + build; ruff + `scripts/check_no_comments.py`; no sqlc surface touched.
- [ ] 5.2 Docs: the three env keys into `backend/agent_py/.env.example` + `README.md` / `README.zh.md` / `README_ALL.md` env tables; `FUTURE_AGENT.md` and `FUTURE_FRONTEND.md` updated (the frame-replay item is the one this change ticks); Redis dependency noted in the agent README.
- [ ] 5.3 Self-review pass over the full diff vs `mvp`: single emit path (no duplicated streaming loop), the write-before-broadcast invariant, the watermark boundary, degradation never claims an unbuffered frame is replayable, no `err.Error()` on the wire, no comments on new lines.
