# Proposal

## Why

The 2026-10-14 incident: the Go chat proxy's SSE client carried a 60s `http.Client.Timeout`, which bounds the entire exchange including the streamed body — every agent run past 60s was severed mid-stream (no `done`, no `error`, no `terminator`; the run cancelled server-side; observed at t+61.8s on a run waiting for an approval). The fix (uncommitted, implemented as part of the incident response) removes the client timeout, bounds the upstream with the request-context deadline (`LLM_MAX_STREAM_DURATION`, default 10m), and closes a cap-cut stream with typed terminal frames. That new wire behavior has no spec coverage: `chat-agent-proxy` only carves a synthesized-frames exception for the unreachable-agent case (`agent_unavailable`).

## What Changes

- ADD a `chat-agent-proxy` requirement: a stream that outlives its wall-clock cap fails closed with typed terminal frames (`error{code:"stream_timeout"}` + `done` + `terminator`) instead of a silently dead connection; the cap MUST be a configurable wall-clock bound on the upstream request, not a fixed client timeout that severs healthy streams; a stream that already forwarded the upstream's own terminal frames MUST NOT be double-terminated.
- No code changes in this change: the implementation already exists in the pending incident fix (`backend/internal/chat/`), with unit/integration tests pinning every scenario. The apply phase verifies the scenarios against those tests and lands them together with the spec text.

## Capabilities

### New Capabilities

(none)

### Modified Capabilities

- `chat-agent-proxy`: the byte-for-byte forwarding contract gains a second, explicitly-bounded carve-out — the stream-duration cap — alongside the existing unreachable-agent branch.

## Impact

- **Code**: none new. The behavior lives in the pending fix (`backend/internal/chat/agent_proxy.go`, `handler.go`, `service.go`, `cmd/api/main.go` + tests: `TestAgentProxy_Integration_StreamCapCutEmitsTypedTerminalFrames`, `TestAgentProxy_Integration_StreamCapAfterUpstreamTerminalEmitsNothing`, `TestNewAgentProxy_DefaultClientCarriesNoStreamTimeout`, `TestSessionsProxy_ForwardBoundedBySessionsTimeout`).
- **Wire**: one new error `code` (`stream_timeout`) on the existing `error` frame shape — permitted by the chat contract's rule that the error frame MAY carry new codes.
- **Docs**: `AgentFaultTolerance.md` row 24 records the fault class (lands with the implementation).
- **Rollback**: revert the pending fix commit; no flag; no rollout gates foreseen (completable entirely in the dev/CI environment, so no sibling rollout change).
