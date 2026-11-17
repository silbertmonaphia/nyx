# Agent Tool Registry Specification

## Purpose

Centralises tool registration, per-owner scoping, input validation, and safe-error mapping behind a single internal registry consumed by both the agent loop and the externally-facing MCP server.

## Requirements

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

### Requirement: Mutating tool schemas declare an optional idempotency key

The mutating feed tools (`create_feed`, `update_feed`, `delete_feed`) SHALL declare an optional `idempotency_key` string argument in their served input schema, with a bounded length, a restricted character set, and matching schema constraints (minimum length and pattern) advertised to clients. The argument SHALL be optional: a caller that omits it receives today's behavior with no deduplication. Read-only tools SHALL NOT declare the argument. The MCP `tools/list` surface SHALL expose the argument so external agents can opt in, and the tool descriptions SHALL document its semantics.

#### Scenario: Mutating tool advertises the optional key

- **WHEN** `tools/list` returns `create_feed`
- **THEN** its `inputSchema` includes an optional `idempotency_key` property, and omitting it is valid

#### Scenario: Read-only tool does not advertise a key

- **WHEN** `tools/list` returns `list_feeds` or `get_feed`
- **THEN** its `inputSchema` does not include `idempotency_key`

#### Scenario: Caller without a key keeps legacy behavior

- **WHEN** an external MCP agent calls `create_feed` without `idempotency_key`
- **THEN** the call dispatches and behaves exactly as before this change

### Requirement: Mutating dispatches are deduplicated per owner and key

When a mutating tool call carries an `idempotency_key`, the registry SHALL record the call and its response atomically with the underlying write: the idempotency record and the data mutation MUST commit in the same Postgres transaction, keyed by the authenticated owner id and the key. A dispatch whose (owner, key) already has a recorded response for the same tool SHALL return that stored response without re-executing the write. A key already recorded for a different tool MUST be rejected with a typed validation error rather than replaying the other tool's response. The stored response MUST be the original successful tool result — including `delete_feed` returning its original success rather than a not-found error on replay. A concurrent duplicate SHALL wait for the first transaction and then return the stored response. Keys MUST be validated for length and character set before the tool body runs; an invalid key MUST return a typed validation error. Deduplication MUST NOT allow one owner to read another owner's stored response.

#### Scenario: Duplicate create returns the original row

- **WHEN** `create_feed` is dispatched twice with the same owner and key and identical arguments
- **THEN** exactly one feed row exists and both calls return the same feed object

#### Scenario: Replayed delete returns success

- **WHEN** `delete_feed` completes successfully and the same owner and key are dispatched again
- **THEN** the second dispatch returns the original success response and does not surface a not-found error

#### Scenario: Concurrent duplicates execute once

- **WHEN** two dispatches with the same owner and key race
- **THEN** one mutation commits, the other returns the same stored response, and no duplicate row is created

#### Scenario: Cross-owner keys are isolated

- **WHEN** two owners present the same key value
- **THEN** each dispatch executes independently and neither can read the other's stored response

#### Scenario: Cross-tool key reuse is rejected

- **WHEN** an owner dispatches `create_feed` and then `delete_feed` with the same key value
- **THEN** the second dispatch returns a typed validation error and does not replay the create response

#### Scenario: Invalid key fails before the tool body

- **WHEN** a caller supplies an `idempotency_key` that violates the declared length or character set
- **THEN** the registry returns a typed validation error and the tool body does not run

#### Scenario: Missing key disables deduplication

- **WHEN** a mutating call omits `idempotency_key`
- **THEN** the mutation executes with today's semantics and no idempotency record is written

### Requirement: Idempotency records are retention-bounded

The stored idempotency records SHALL be purged after a bounded retention window of at least one hour. Purging MUST only end the deduplication window; it MUST NOT modify tool data or leave a partially applied mutation.

#### Scenario: Expired records are purged

- **WHEN** the retention cutoff passes for an idempotency record
- **THEN** the record is deleted and the underlying feed data is unchanged
