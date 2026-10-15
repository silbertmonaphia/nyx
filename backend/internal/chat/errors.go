package chat

import "errors"

// ErrAgentUnavailable is returned by AgentProxy.Run when the Python
// LangGraph orchestrator (PYTHON_AGENT_URL) is unreachable or
// returns 5xx. The chat handler translates this into a single
// `event: error\ndata: {"code":"agent_unavailable", ...}\n\n`
// frame followed by `event: done` + `event: terminator` (the
// "fail closed on Python down" contract from
// openspec/changes/agent-langgraph/specs/chat-agent-proxy/spec.md).
//
// The sentinel is intentionally generic — Python being down,
// DNS failing, the dial timing out, or the upstream returning
// HTTP 5xx are all unified under one error so the handler has a
// single branch to handle. The chat wire contract does not
// distinguish them; from the SPA's perspective the agent path is
// unavailable, full stop.
//
// Wire contract: see the spec's "Python service unavailable fails
// closed" requirement — the handler MUST NOT silently fall back
// to the single-turn path, because that would hide agent-side
// failures from the operator and from the user's AGENT_ENABLED=true
// expectation.
var ErrAgentUnavailable = errors.New("chat: python agent service is unavailable")
