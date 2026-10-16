# Tasks

## 1. Policy module (load, validate, match)

- [ ] 1.1 Create `agent/tool_policy.py`: pydantic schema for the ordered rule list (`id`, `action: allow|deny`, `tool` glob, optional `args` glob) + `load_policy(path)` returning an immutable snapshot; malformed JSON, unknown action, missing `id`, or unreadable file raise a policy-config error. Verify: unit tests for valid load, each invalid shape, and unreadable path.
- [ ] 1.2 Implement first-match-wins matching: `fnmatchcase` on tool name and on the canonical args serialization; omitted `args` matches anything; `allow` short-circuits; no match returns "no decision". Verify: unit tests for name glob, args glob, allow-before-deny ordering, deny-before-allow ordering, no-match.
- [ ] 1.3 Pin the serializer contract: the matcher consumes the exact canonical-args function the confirmation gate keys on (one shared producer, not a parallel re-serialization). Verify: unit test asserting policy key == approval gate key for identical calls.

## 2. Settings + startup wiring

- [ ] 2.1 Add `agent_tool_policy_path: str | None = None` to `Settings`. Verify: settings unit test for the unset default.
- [ ] 2.2 Wire startup: when the path is set, load + snapshot once during service wiring; a policy-config error aborts the boot with the policy problem named. When unset, no policy object is constructed. Verify: unit tests — invalid path exits, unset path constructs nothing.

## 3. Dispatch-funnel integration

- [ ] 3.1 Evaluate policy in `_dispatch_with_retries` ahead of the confirmation-gate block; on deny, emit the closed outcome through the existing `tool_result{ok:false}` path (no dispatch, no confirm card, no retry consumption) and return the fixed denial text constant. Verify: executor unit tests — deny by name, deny by args, deny suppresses confirmation for a mutating tool, no `confirm_request` event appended.
- [ ] 3.2 Preserve today's behavior when policy is unset or no rule matches: confirmation flow, retry taxonomy, and local-tool dispatch unchanged. Verify: executor unit tests — unset policy mutating call still confirms; allow-rule match proceeds to confirmation; no-match registry call proceeds.
- [ ] 3.3 Cover both tool sources and the outage path: policy denies an agent-native tool call and a registry call during a `tools/list` failure. Verify: executor unit tests for both, including the degrade-to-local run.
- [ ] 3.4 Assert the denial text is one static constant across rules and calls and reaches both the `tool_result` frame and the react-loop `ToolMessage` verbatim. Verify: wire-contract test parsing the emitted frame payload.

## 4. Audit

- [ ] 4.1 Add `agent_tool_policy_decisions_total{tool, decision, rule}` counter + structured log line on the agent logger tree; record no argument content. Verify: unit test asserting counter labels and caplog line contains rule id + tool name but no arg values.

## 5. Security evals + docs

- [ ] 5.1 Add policy-gate cases to `evals/` security loader: an injected destructive-tool prompt against a policy that denies that tool must yield the blocked tool result, with the payload-only delivery marker asserted present (anti-vacuity convention). Verify: security suite green against the bundled stub.
- [ ] 5.2 Document `AGENT_TOOL_POLICY_PATH` + rule file format on the doc surfaces (backend/agent_py README + README.zh, root READMEs per the per-feature What-it-does/Endpoints/Key-env-vars convention). Verify: grep the feature identifier across surfaces finds all updated docs.

## 6. Integration

- [ ] 6.1 Full `backend/agent_py` pytest green in both modes (with and without `TEST_ASYNC_PG_DSN`). Verify: both runs exit 0.
- [ ] 6.2 `openspec validate --strict` on the change passes; `make sqlc-diff` clean (Go untouched, drift check is the guard). Verify: both commands exit 0.
- [ ] 6.3 E2E: `@live` Playwright spec — with the policy enabled on the side stack, a prompt that drives the denied tool shows the policy-denied tool result in the agent-steps region (serial `--workers=1` per live-suite conventions). Verify: new spec green in `npm run test:e2e:live`.
- [ ] 6.4 Tick the landed roadmap item in `FUTURE_AGENT.md`. Verify: the checkbox is ticked in the diff.
