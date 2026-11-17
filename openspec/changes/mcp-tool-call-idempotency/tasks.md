# Tasks

## 1. Schema and tool contract

- [x] 1.1 Add `backend/migrations/0000NN_tool_idempotency.{up,down}.sql` creating `tool_idempotency` with PK `(owner_id, key)` plus the `created_at` index; verify `cd backend && go test ./internal/platform/database/...` boots it against a testcontainer and the down migration drops it
- [x] 1.2 Add `backend/queries/tool_idempotency.sql` (insert-if-absent, select response, delete older than cutoff) and regenerate with `cd backend && make sqlc`; verify `make sqlc-diff` is clean
- [x] 1.3 Add optional `IdempotencyKey` to `CreateFeedArgs` / `UpdateFeedArgs` / `DeleteFeedArgs` with length/charset validation, matching minimum-length/pattern schema constraints, and property-description guidance for external clients (the tool description must not mention the key); verify `cd backend && SKIP_CONTAINERS=true go test ./internal/tool/...` covers valid, absent, invalid, and that the field stays optional
- [x] 1.4 Verify the `tools/list` contract exposes `idempotency_key` as optional on the three mutating feed tools and absent on `list_feeds` / `get_feed`; extend `backend/internal/mcp/tools_list_contract_test.go` and run it
- [x] 1.5 Strip `idempotency_key` from the model-bound function schema and retry re-prompt schema text while the dispatcher decides injection from the raw served schema; verify tests assert the model never sees the property and the derived key still reaches dispatch

## 2. Postgres-enforced deduplication

- [x] 2.1 Implement `idempotency.Store.Do`: open one `pgx.Tx`, insert-if-absent, run the mutation bound to the same tx via `feed.NewRepositoryFromQuerier(q.WithTx(tx))`, persist the response, and return the stored response on conflict; verify with a real-Postgres integration test that a concurrent duplicate executes the mutation once and both callers receive the same response
- [x] 2.2 Add `CreateFeedIdempotent` / `UpdateFeedIdempotent` / `DeleteFeedIdempotent` to the feed service, each delegating to `Store.Do` and running embedding indexing + cache invalidation only after commit (skipped on replay), with the tool layer type-asserting the narrow interface; verify duplicate create yields one row, replayed delete returns the original success, cross-owner identical keys stay isolated, and a replay does not call the embedding provider
- [x] 2.3 Map an invalid `idempotency_key` and a key reused for a different tool to typed validation errors before any write; verify with tool-level tests asserting the safe-error funnel output and no replay of the other tool's response
- [x] 2.4 Add the retention cleaner (`TOOL_IDEMPOTENCY_RETENTION`, default 7 days) as a periodic Go job and cover it with a test using a short retention; verify expired rows are deleted while feed rows are untouched
- [x] 2.5 Verify legacy behavior: a call without `idempotency_key` writes no idempotency row and behaves exactly as before; test at the tool and MCP handler layers

## 3. Python agent key derivation

- [x] 3.1 Derive the key once per intent as `uuid5(run_id, turn_id, step_id, tool, canonical_args)` with a checkpointed per-turn id, reusing the executor's `_canonical_args`; verify unit tests for transient retry stability, corrected-argument key change, resume-time reproducibility, and distinct keys for identical turns in one session
- [x] 3.2 Inject the key only when the raw served tool schema declares `idempotency_key`, overriding any model-supplied value, while the model-bound schema omits the property; verify non-declaring tools dispatch unchanged arguments and the model's arguments never contain the key
- [x] 3.3 Keep `call_id` per attempt and frame semantics unchanged; verify a retry emits distinct `call_id` values while the arguments carry the same key
- [x] 3.4 Add the cross-runtime wire-contract test: serialize the agent-produced arguments, parse them with the real Go `CreateFeedArgs` + validation, and dispatch through the registry; verify a duplicate key produces one write
- [x] 3.5 Document the key semantics, the external-client guidance, the model-hidden property, and the react-step limitation in `backend/agent_py/README.md` and `FUTURE_AGENT.md`; verify the documented claims match the tests

## 4. E2E and gates

- [x] 4.1 Add an env-gated, test-only fault seam that commits a mutating tool call and then loses the response, so the executor's transient retry path runs deterministically; verify production default is off and a unit test pins the gate
- [x] 4.2 Add a Playwright spec that sends a chat message triggering `create_feed`, injects the response-loss fault, and asserts the UI reports one successful creation while the feeds API shows exactly one row, plus a same-session second identical turn that must create a second row; verify both specs pass with `npx playwright test tests/e2e/chat-tool-idempotency.spec.ts --workers=1` against a side stack started with `PYTHON_AGENT_TEST_LOSE_TOOL_RESULT_ONCE=true` and `E2E_TOOL_IDEMPOTENCY_FAULT=true`
- [x] 4.3 Run the full gates: `cd backend && make test`, `make lint`, `make sqlc-diff`, `make coverage-check`, `make -C backend/agent_py lint`, `make -C backend/agent_py test-cov`, and `cd frontend && npm run test:e2e`; record any environment-blocked gate in the change instead of checking it
