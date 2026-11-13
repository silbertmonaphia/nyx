# Tasks

## 1. Python store — paged replay read

- [ ] 1.1 Add `ChatStore.list_messages_page(session_id, user_id, *, limit, before)` per design D3 (PK keyset, `LIMIT limit+1` has_more probe, ownership probe + transaction reused, page reversed to chronological, `next_before` = `str(oldest id)`); verify with pytest unit tests: full-window page, `has_more` true/false, empty page for a cursor below the first message, cursor round-trip encode/decode, owner-scoping 404 parity (`None` on wrong user / soft-deleted), no interference with existing `list_messages`
- [ ] 1.2 Verify the existing `backend/agent_py` pytest suite passes both with and without `TEST_ASYNC_PG_DSN` set (skip-gate check, bare-config run per repo convention): `cd backend/agent_py && make test` plus one bare-config run

## 2. Python API — pagination params on the messages endpoint

- [ ] 2.1 Extend `GET /agent/sessions/{session_id}/messages` in `agent/api/chat_sessions.py` to parse `limit` (int in [1,200], else 400) and `before` (decimal-string cursor this endpoint mints, else 400), emit `has_more` / `next_before` (`null` when `has_more` false), and keep the no-param response byte-shape identical to today; verify with pytest API tests: no-param parity (no new keys), newest-window ordering oldest-first, cursor continuation assembles the older page, each invalid-params case 400s, cross-user and soft-deleted 404 with params present
- [ ] 2.2 Export a captured response fixture (e.g. `frontend/src/features/chat/services/__fixtures__/messages_page.json`) from the real serializer output so the frontend consumer test parses actual wire bytes, and verify the file carries `messages` + `has_more` + string `next_before`
- [ ] 2.3 Run `cd backend/agent_py && make lint` (ruff + mypy baseline diff); if the baseline fires on the new store/API signatures, regenerate with `make mypy-baseline` and commit it alongside

## 3. Go proxy — pin the query passthrough

- [ ] 3.1 Add a `sessions_proxy_test.go` case asserting `GET /api/chat/sessions/{id}/messages?limit=50&before=<cursor>` reaches the upstream URL with the query string verbatim (no re-encoding, per the existing cursor comment in `Forward`); verify with `cd backend && SKIP_CONTAINERS=true go test ./internal/chat/`
- [ ] 3.2 Run `cd backend && SKIP_CONTAINERS=true go build ./... && go test ./internal/chat/` to confirm no Go code changed behavior beyond the new test

## 4. Frontend service + store — paging state and refresh wiring

- [ ] 4.1 Extend `chatService.getMessages` with `{limit, before}` opts and coerce `has_more` / string `next_before` (absent → full-replay mode, `hasMore=false`) ; verify via vitest parsing the `messages_page.json` fixture through the real coercion (consumer-side wire test)
- [ ] 4.2 Parallelize `loadSessionInto` (D5): resume catch-and-log concurrent with the replay fetch; verify vitest: replay renders when resume rejects, and when resume is slow the replay is not queued behind it
- [ ] 4.3 Add store paging state (`replayNextBefore`, `replayHasMore`, `isFetchingOlder`, `isLoadingReplay` skeleton flag) and `loadOlderMessages` with single-flight + `replaySeq` epoch guard; verify vitest: prepend order, duplicate-page guard, session-switch discards in-flight page, live-stream attach supersedes an in-flight prepend (attachedToken guard)
- [ ] 4.4 Make the detached-run terminal refresh pagination-aware (D4 merge + gap fallback to page-1 reset); verify vitest: merge keeps older pages and appends the new turn, gap case resets to the fresh latest page, non-paginated (legacy full) replay still refreshes as today, and every merge outcome is asserted contiguous (no id gaps between the fresh page and retained older pages — 49/50/51-message boundary cases)
- [ ] 4.5 Add the visibility refresh (D6): `visibilitychange`/`focus` → throttled (30s) single-flight `refreshSessionList`; verify vitest: appears on visible/focus, second trigger inside the window dropped, failure quiet with rows retained, no timer polling

## 5. Frontend components — skeleton, scroll-up, listener

- [ ] 5.1 `ChatPanel`: render skeleton rows while `isLoadingReplay` (not the welcome state), and the scroll-up sentinel with scrollTop/scrollHeight compensation pinning; verify component tests: skeleton shows during fetch and clears on every exit path (success render, 404 clear, non-404 failure, session switch), prepend keeps the viewed message in the viewport, sentinel fires `loadOlderMessages` near the top and stops at `has_more=false`
- [ ] 5.2 `ChatSidebar`: attach the visibility listener at mount (dialog open ⇒ active); verify component test: listener registered/unregistered with mount, refresh call observed on a simulated visibility change
- [ ] 5.3 Run `cd frontend && npm run lint && npm test` — full suite green

## 6. E2E — agent-free Playwright on the seeded stack

- [ ] 6.1 Add `frontend/tests/e2e/chat-history-pages.spec.ts` (agent-free, joins `npm run test:e2e` / CI): seed >page-size messages via the sessions API, then cover — click → skeleton → latest page renders; scroll-up prepends older without viewport jump; second burst adds no duplicates; `has_more=false` terminal stops fetching; cross-tab session appears in the rail after visibility regains without reload; verify green against the dev side stack (`E2E_API_URL` conventions, infra up first)
- [ ] 6.2 Confirm the existing `chat-history-sidebar.spec.ts` and `chat-step-replay.spec.ts` still pass unchanged (backward-compat regression), plus the full `npm run test:e2e` suite
- [ ] 6.3 Verify the no-param contract end-to-end: a raw fetch of `/api/chat/sessions/{id}/messages` without params through the running stack returns every message with no `has_more` key (old-client parity pin)

## 7. Docs

- [ ] 7.1 Grep the previous analogous feature's identifier (`chat/sessions`) to enumerate every doc surface; update README.md / README.zh + `backend/agent_py/README.md` endpoint sections with the new optional params and response fields (What-it-does / Endpoints / Key-env-vars structure per repo convention)
- [ ] 7.2 Tick the matching roadmap items in `FUTURE*.md` if the perf/freshness work is listed there; run the drift-check skill before committing
