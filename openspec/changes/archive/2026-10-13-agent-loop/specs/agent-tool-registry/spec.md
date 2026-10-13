# Spec Delta

## Purpose

Centralises tool registration, per-owner scoping, input validation, and safe-error mapping behind a single internal registry consumed by both the agent loop and the externally-facing MCP server.

## ADDED Requirements

### Requirement: Tools register through one source of truth

A tool SHALL be registered once, with its name, version, JSON-schema-derived argument type, and dispatcher. The registry is the only place tool metadata lives — both the agent loop and the MCP server MUST consult the registry rather than hold their own tool definitions.

#### Scenario: Same tool available to agent and MCP

- **WHEN** a tool is registered with name `list_feeds`
- **THEN** the agent loop can invoke `list_feeds` by name AND the MCP server exposes a tool named `list_feeds` with identical arguments and identical dispatch semantics — no duplicated metadata

#### Scenario: Registering a tool once surfaces everywhere

- **WHEN** a new tool is added through the registry's registration API
- **THEN** the next MCP discovery response lists the new tool, and the next agent loop that consults the registry can dispatch to it — without separate MCP or agent-side code changes

### Requirement: Per-owner scoping is mandatory

Every tool dispatch SHALL run with an authenticated user identifier on the request context. A dispatch without an authenticated user identifier MUST fail closed before the tool body executes. Tools that operate on per-user data MUST read the user identifier from the request context, never from arguments supplied by the caller.

#### Scenario: Missing user identifier fails closed

- **WHEN** a tool is invoked without a user identifier on the context
- **THEN** the registry returns a typed ownership error and the tool body does not execute

#### Scenario: Tool reads user from context, not arguments

- **WHEN** a tool needs the owning user's identifier to enforce row-level access
- **THEN** it reads the identifier from the request context; arguments named like `user_id` are ignored as authoritative

### Requirement: Input arguments are validated before dispatch

Tool arguments SHALL be validated against the tool's declared argument type before the tool body runs. Validation failures MUST return a typed validation error that includes the failing field; the tool body MUST NOT run on invalid input.

#### Scenario: Invalid arguments fail before tool body

- **WHEN** a caller invokes a tool with arguments that fail struct-tag validation
- **THEN** the registry returns a typed validation error and the tool's dispatcher is never invoked

### Requirement: Tool errors map through the safe-error funnel

A tool that returns or panics with an internal error MUST have that error translated through the project's safe-error funnel before any error text is returned to the caller (MCP wire or agent SSE). Internal error strings (SQL fragments, bcrypt strings, JWT parser output, panic traces) MUST NOT appear in the response.

#### Scenario: Tool returns a sentinel error

- **WHEN** a tool returns a known domain sentinel (e.g. `feed.ErrNotFound`)
- **THEN** the caller receives the mapped public form; the underlying sentinel text does not leak

#### Scenario: Tool returns an unknown error

- **WHEN** a tool returns an unclassified error
- **THEN** the caller receives a static safe detail; the wrapped error is logged with the request ID at warn level

#### Scenario: Tool panics

- **WHEN** a tool panics during dispatch
- **THEN** the registry recovers, the caller receives a typed internal error, and the panic stack is logged with the request ID — never returned to the caller

### Requirement: MCP server and agent loop share the registry

The MCP server's tool surface MUST be a thin projection over the registry: tool discovery lists what the registry exposes, tool invocations dispatch through the registry's dispatcher, and the MCP wire shape is the only thing the MCP handler owns.

#### Scenario: MCP discovery reflects registry state

- **WHEN** the MCP server handles `tools/list`
- **THEN** the response lists exactly the tools currently registered, with no hardcoded MCP-side tool list drifting from the registry

#### Scenario: Agent invokes a registry tool

- **WHEN** the agent loop dispatches a tool by name
- **THEN** the same registry dispatcher the MCP server uses handles the call — including per-owner scoping, validation, and safe-error mapping