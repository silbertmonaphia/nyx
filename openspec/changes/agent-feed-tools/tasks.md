# Tasks

## 1. Go — annotate the MCP feed tools

- [x] 1.1 Annotate the five feed tools in `internal/mcp/service.go` (`list_feeds`/`get_feed`: `WithReadOnlyHintAnnotation(true)`; `create_feed`/`update_feed`/`delete_feed`: `WithReadOnlyHintAnnotation(false)` + `WithDestructiveHintAnnotation(true)`) — verify `cd backend && go test ./internal/mcp/...` passes and existing name/description/inputSchema assertions are unchanged.
- [x] 1.2 Extend the MCP service/handler test to assert the tools/list response carries the expected `annotations` per tool — verify the new assertions fail against the un-annotated code, then pass.
- [x] 1.3 Freeze the tools/list wire (native `inputSchema` + `annotations`) as a checked-in cross-runtime fixture (`backend/agent_py/tests/fixtures/mcp_tools_list.json`) plus a Go test asserting the server's live response matches it — verify the golden test passes and the fixture parses as JSON with all five tools.

## 2. Python — MCP-native schema normalization

- [x] 2.1 Normalize registry-sourced schemas at the dispatcher boundary (`tools_client.py` / `tool_dispatch.py`): `inputSchema` → OpenAI `parameters`, annotations preserved per tool on a side record, unbindable entries logged and skipped — verify unit tests consuming the captured Go fixture show the bound function carries the real parameters and the annotations are reachable.
- [x] 2.2 Fix `_tool_schema_text` in `executor.py` to read the normalized shape — verify the retry re-prompt test asserts a non-empty parameter schema for an MCP-delivered tool (fails against the un-normalized shape).
- [x] 2.3 Guard the consumer-parser contract: the normalization tests MUST source their input from the checked-in Go fixture (no hand-written OpenAI dicts) — verify mutating the fixture's `inputSchema` breaks the bound-parameters assertion.
- [x] 2.4 Memoize the registry `tools/list` per dispatcher instance (per run, per bearer): the merged view, registry-only view, and annotation map all derive from one fetch — verify with a transport double that two `list_tools()` calls hit the transport once and the annotation map is still populated.

## 3. Python — dynamic planner catalog

- [x] 3.1 Expose the registry-only tool view on the dispatcher (merged list minus agent-native tools) — verify a unit test shows local web tools are excluded and registry entries included.
- [x] 3.2 Build the planner "Registry tools available" block from that view at plan time; omit the block when the registry call fails or returns empty — verify unit tests: block lists the fixture tools with names; prompt equals the pre-change text when the list raises or is empty.
- [x] 3.3 Thread the dispatcher into the planner node (graph factory change mirroring the executor factory) — verify planner + graph wiring tests pass with the new parameter.

## 4. Python — confirmation gate and decision broker

- [x] 4.1 Approval decision store module (Redis): keys `approval:{run_id}:{approval_id}`, `SET NX` first-wins, waiter poll (~0.5 s), key TTL = approval TTL + margin — verify unit tests (Redis double) for first-wins, duplicate no-op, expiry, and store-unavailable.
- [x] 4.2 Gate inside `_dispatch_tool_with_retries`: confirm when annotations do not assert `readOnlyHint=true`; deny / expiry / store-unavailable close the call as a model-visible denial result (no dispatch, no retry-counter increment); approved calls dispatch unmodified without re-confirmation on transient retry — verify per-branch unit tests and a react-loop gate test.
- [x] 4.3 Emit the `confirm_request` frame (run id, step id, tool, masked args, approval id, expiry) and SSE comment keep-alives (~15 s) while waiting — verify frame-shape and keep-alive unit tests.
- [x] 4.4 `POST /agent/approvals` endpoint: `X-Nyx-User-Id` 400 contract, run-ownership via run metadata (single-sentinel 404), idempotent duplicates, typed 404 for unknown/expired approval ids — verify endpoint tests cover each case and no internal error text reaches the body.
- [x] 4.5 `PYTHON_AGENT_APPROVAL_TTL_SECONDS` (default 300, refuse < 1) in config, documented in `backend/agent_py/.env.example` + `README.md` — verify config tests and the startup-refusal test.

## 5. Go — approval route

- [x] 5.1 `POST /api/chat/approvals` in `internal/chat`: standard JWT middleware, forward body to `PYTHON_AGENT_URL/agent/approvals` with `X-Nyx-User-Id`, upstream status/body passthrough, fail closed when disabled/unreachable, per-user rate limit, token never logged — verify handler tests for forwarded headers, passthrough, 401 without JWT, agent-down error, and the no-token-in-logs assertion.
- [x] 5.2 Wire the route in `cmd/api/main.go` alongside the chat routes — verify `cd backend && go build ./... && go test ./internal/chat/...` pass.

## 6. Frontend — confirmation affordance

- [x] 6.1 Parse the `confirm_request` frame in `services/chatService.ts` (+ types/store plumbing) — verify a parser unit test consuming a captured Python frame fixture; unknown-kind skipping by old parsers stays covered.
- [x] 6.2 Approve/deny affordance in the chat turn rendering tool + args, POSTing the decision, and rendering the denial result in-line — verify component tests for render, approve call, deny call, and denied-result rendering.
- [x] 6.3 Verify `cd frontend && npm test && npm run lint && npm run build` pass.

## 7. Integration — e2e and repo gates

- [x] 7.1 Playwright spec `frontend/tests/e2e/chat-feed-approval.spec.ts` with canned SSE fixtures routed into `/api/chat`: approve flow shows the card → posts the decision → renders the continued stream; deny flow shows no mutation; `list_feeds` renders with no card — verify `cd frontend && npm run test:e2e` passes.
- [ ] 7.2 Live dev-stack smoke (header-documented in the spec): `make up` + `make dev` with MCP enabled, ask the chat to create a feed, approve in the browser, confirm the feed appears on the feeds page; record the observed result in this change — verify the recorded steps match the shipped UI.
- [x] 7.3 Update `FUTURE_AGENT.md` to mark the feed-tool planner awareness + approval gate as landed (with the flag prerequisites) — verify the entry matches the implemented behavior.
- [ ] 7.4 Full-suite gate: `cd backend && go test ./...`, `golangci-lint run --timeout=5m`, agent pytest suite, and the frontend suites — verify all green (note in this change if the environment forces `SKIP_CONTAINERS=true`).
