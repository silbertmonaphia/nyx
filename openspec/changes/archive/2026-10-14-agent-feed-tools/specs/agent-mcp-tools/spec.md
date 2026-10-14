# Spec Delta

## Purpose

Defines how the chat agent consumes the MCP registry's tool surface: normalizing the served schemas into the shape the model is bound to, advertising that surface to the planner at plan time, and requiring explicit user confirmation before any call the registry does not mark as read-only.

## ADDED Requirements

### Requirement: Registry tool schemas are normalized to the bindable shape

The agent SHALL normalize every tool definition returned by MCP `tools/list` from the MCP-native shape (`{name, description, inputSchema, annotations}`) into the function-tool shape the model is bound to, mapping `inputSchema` onto the bound function's parameter schema. Normalization MUST apply uniformly to react-loop binding and to the argument-schema text the executor composes for retry re-prompts. An entry the agent cannot bind (missing name, or a schema that is not an object) SHALL be logged and skipped without failing the run. Tool annotations MUST be preserved for the confirmation policy even though they are not part of the bound function schema, and an absent or empty `annotations` object MUST NOT crash normalization.

#### Scenario: Bound MCP tool carries its argument schema

- **WHEN** the registry serves `list_feeds` with a native `inputSchema` declaring `query` / `page` / `page_size` / `order`
- **THEN** the function the model is bound to exposes those arguments as its parameter schema — the schema is not silently dropped in conversion

#### Scenario: Retry re-prompt shows the real schema

- **WHEN** a registry tool call is rejected and the executor re-prompts the model with the tool's argument schema
- **THEN** the re-prompt text carries the tool's actual parameters, not an empty object

#### Scenario: Unbindable registry entry is skipped, not fatal

- **WHEN** a registry entry lacks a name or its `inputSchema` is not an object
- **THEN** the entry is logged and omitted from the bound tool list; the run continues with the remaining tools

### Requirement: Planner catalog mirrors the live registry

The planner prompt SHALL include a block listing the registry tools available to the run — one line per tool carrying its name and description — derived from the same merged registry list the executor binds, with no hardcoded tool names. The block SHALL be omitted when the registry list call fails or returns no tools, so in that case the planner prompt is identical to the pre-change prompt. Agent-native (local) tools SHALL NOT be duplicated in this block — they keep their own catalog.

#### Scenario: Served tools are advertised to the planner

- **WHEN** MCP `tools/list` returns the five feed tools
- **THEN** the planner prompt lists their names and descriptions, and the planner can emit a `tool` step naming `list_feeds` or `create_feed`

#### Scenario: Registry outage leaves planning unchanged

- **WHEN** the registry `tools/list` call fails, or succeeds with an empty tool list
- **THEN** no registry-tool block appears in the planner prompt and planning proceeds exactly as before the change

### Requirement: Mutating calls require user confirmation

Before dispatching a registry tool whose annotations do not assert `readOnlyHint=true` — annotation-absent tools included (fail-safe) — the agent SHALL pause at the shared dispatch funnel (identical for plan `tool` steps and react loops) and emit a `confirm_request` frame. The original call MUST NOT reach the registry until an `approve` decision for that approval id is recorded. A `deny` decision SHALL leave the tool unexecuted and deliver a model-visible denial result (tool name + denial reason) so the model can continue or adapt — denial MUST NOT be classified as a transient or correctable failure and MUST NOT increment any tool-retry counter. When the approval TTL expires with no decision, the call SHALL be treated as denied (expiry reason) on the same path — never auto-approved. Tools whose annotations assert `readOnlyHint=true` SHALL dispatch without confirmation.

#### Scenario: Read-only call runs unconfirmed

- **WHEN** a `tool` step or react loop calls `list_feeds` (annotated read-only)
- **THEN** the call is dispatched immediately and no `confirm_request` frame is emitted

#### Scenario: Mutating call pauses for confirmation

- **WHEN** a step calls `create_feed` (annotated not read-only)
- **THEN** the stream emits a `confirm_request` frame carrying the tool name, arguments, approval id, and expiry, and no request reaches the registry until a decision arrives

#### Scenario: Approve dispatches the original call

- **WHEN** an `approve` decision is recorded for the pending approval id
- **THEN** the original call is dispatched unmodified and its regular `tool_call` / `tool_result` frames are emitted in the same step

#### Scenario: Deny leaves data untouched and informs the model

- **WHEN** a `deny` decision is recorded for a pending `delete_feed` call
- **THEN** no request reaches the registry, the model receives a denial result naming the tool and reason, and the run continues with no retry-counter increment

#### Scenario: Expiry is a denial, not a retry

- **WHEN** no decision is recorded before the approval's expiry
- **THEN** the pending call is closed as denied with an expiry reason and the model is informed — the call is never auto-approved and never dispatched

#### Scenario: React loop is gated identically

- **WHEN** the model inside a react step emits a mutating registry call
- **THEN** the same pause / confirmation / resume semantics apply as for a plan `tool` step

### Requirement: Approval decisions are brokered across replicas and fail closed

Approval decisions SHALL be recorded in the shared decision store (the agent's Redis connection) keyed by run id and approval id, so a decision delivered to any agent replica resolves the waiting dispatch. The first decision recorded for an approval id wins; any later duplicate decision MUST NOT change the outcome. When the decision store is unavailable — at pause time or while waiting — the pending mutating call MUST NOT execute: the call SHALL be closed as denied with an availability reason and the model informed. No failure mode SHALL result in an implicit approval.

#### Scenario: Decision from another replica resolves the wait

- **WHEN** a run is awaiting approval on replica A and the decision is delivered to replica B
- **THEN** replica A resolves the wait with that decision and proceeds accordingly

#### Scenario: Duplicate decision does not flip the outcome

- **WHEN** a second decision for an already-decided approval id is recorded
- **THEN** the recorded outcome is unchanged — an approve after a deny does not dispatch the call

#### Scenario: Broker outage blocks the mutation

- **WHEN** the decision store is unreachable while a mutating call is paused or waiting
- **THEN** the call is closed as denied with an availability reason, the model is informed, and no request reaches the registry
