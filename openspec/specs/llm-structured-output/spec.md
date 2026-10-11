# Llm Structured Output Specification

## Purpose

Lets the LLM provider seam carry tool calls (and later structured output) between the backend and upstream chat-completion providers, with identical behaviour for OpenAI and vLLM.

## Requirements

### Requirement: Provider request accepts tool definitions

The provider seam SHALL accept an ordered list of tool definitions and an optional tool-choice directive on a chat request. A request with zero tool definitions and an empty tool-choice SHALL behave exactly as a pre-change single-turn text request — no extra calls, no extra frames.

#### Scenario: Zero-tools request behaves like pre-change

- **WHEN** a caller invokes `Chat` with an empty tool list and no tool-choice
- **THEN** the seam behaves identically to a request without these fields: same model selection, same single-stream of content deltas, same terminal usage frame, same error semantics

#### Scenario: Tool definitions threaded to the upstream provider

- **WHEN** a caller invokes `Chat` with one or more tool definitions
- **THEN** the request forwarded to the upstream chat-completion endpoint carries the same tools in order, plus the supplied tool-choice

### Requirement: Message carries tool-call and tool-result fields

The message envelope SHALL support a `tool` role that references a prior tool call by identifier and carries the tool's textual result. An `assistant` message SHALL be able to carry one or more tool-call invocations alongside or instead of plain content.

#### Scenario: Assistant message emits a tool call

- **WHEN** an `assistant` message carries one or more tool-call invocations
- **THEN** the upstream wire format preserves the tool-call identifiers, names, and JSON arguments

#### Scenario: Tool message replies with a result

- **WHEN** a `tool` message is appended after an assistant tool call, identified by the call's identifier and the originating tool's name
- **THEN** the upstream provider accepts the message as the result of that specific tool call and continues the conversation

### Requirement: Streaming frames distinguish content from tool calls

The streaming callback SHALL deliver frames that distinguish plain content deltas, tool-call deltas (name / arguments / identifier), terminal usage, and provider notes. Callers MUST be able to reconstruct both the textual reply and the full set of tool calls invoked in a single turn.

#### Scenario: Mixed content and tool-call stream

- **WHEN** the model returns a turn that contains both reasoning content and one or more tool calls
- **THEN** the caller receives content frames for the textual portion and tool-call frames for each invocation, in the order the model emitted them, terminating with a single usage frame

#### Scenario: Pure tool-call turn has no stray content

- **WHEN** the model returns a turn consisting only of tool calls and no content
- **THEN** the caller receives tool-call frames only; the assembled reply contains zero content characters

### Requirement: OpenAI and vLLM clients both surface tool calls

Both the OpenAI and the vLLM client SHALL surface tool-call deltas through the streaming callback. The vLLM client MUST NOT silently drop tool-call deltas — any vLLM wire-shape divergence from OpenAI is mapped to the same `Delta` shape the caller sees.

#### Scenario: OpenAI canned SSE replay surfaces tool calls

- **WHEN** the OpenAI client is fed a canned SSE sequence that includes tool-call deltas
- **THEN** the test harness observes the same `Delta` shape and tool-call identifiers that an OpenAI live response would produce

#### Scenario: vLLM canned SSE replay surfaces tool calls

- **WHEN** the vLLM client is fed a canned SSE sequence that includes tool-call deltas in vLLM's wire shape
- **THEN** the test harness observes the same `Delta` shape and tool-call identifiers that a vLLM live response would produce — no deltas silently dropped

### Requirement: Provider failover honours tool-call parity

The provider failover router SHALL only route a tool-bearing request to a fallback provider when that provider has demonstrated tool-call parity (i.e. the tool-call integration test for that provider passes in the current build). Until parity is verified for vLLM, the router SHALL refuse tool-bearing requests on the unverified provider rather than fail them open.

#### Scenario: Unverified provider refuses tool-bearing request

- **WHEN** a tool-bearing request is routed to a provider whose tool-call integration test is currently red in CI
- **THEN** the router returns a typed error indicating the provider does not yet support tool calls, rather than forwarding the request and silently dropping tool-call deltas

#### Scenario: Both providers verified — failover proceeds

- **WHEN** both providers' tool-call integration tests are green
- **THEN** a transient failure on the primary provider triggers a clean retry on the secondary without dropping any in-flight tool-call deltas the caller has already received