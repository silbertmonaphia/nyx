# Tasks

## 1. Scenario verification against the implemented fix

- [x] 1.1 Trace each scenario to its pinning test: cap-mid-stream → `TestAgentProxy_Integration_StreamCapCutEmitsTypedTerminalFrames`; no-double-termination → `TestAgentProxy_Integration_StreamCapAfterUpstreamTerminalEmitsNothing`; healthy-stream-untouched → `TestAgentProxy_Integration_HandlerForwardsPythonBytesByteForByte`; disconnect-precedence → the request-context cancel path exercised in `TestAgentProxy_NetworkError_HangsUntilClientGivesUp`; verify with `cd backend && SKIP_CONTAINERS=true go test ./internal/chat/ -count=1 -v` (all pass, scenarios traced to assertions)
- [x] 1.2 Verify the cap wiring and the client-timeout removal stay pinned: `TestNewAgentProxy_DefaultClientCarriesNoStreamTimeout` (no client Timeout, response-header guard) and the handler's cap-bounded stream test; verify `grep -n "stream_timeout" AgentFaultTolerance.md` shows row 24 landing with the implementation
- [x] 1.3 Record any scenario the trace surfaces as unpinned as a follow-up test in this group before the change is applied; verify by re-running 1.1's command — trace found all four scenarios pinned; no gap

## 2. Integration

- [x] 2.1 Confirm the spec text and the fix land as one reviewable unit: `git status` shows `openspec/changes/agent-stream-timeout-frame/` alongside the fix paths (`backend/internal/chat/`, `backend/cmd/api/main.go`, `AgentFaultTolerance.md`), and the live specs pass on the frozen revision (`PLAYWRIGHT_BASE_URL=http://localhost:5274 npx playwright test tests/e2e/chat-long-run-survival.spec.ts tests/e2e/chat-feed-approval-live.spec.ts --workers=1`)
