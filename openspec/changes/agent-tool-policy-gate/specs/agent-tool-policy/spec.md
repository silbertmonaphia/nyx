# Spec Delta

## Purpose

Operator-configured deny rules for agent tool calls: a policy layer that flat-blocks matching tool calls before they dispatch or reach the user-confirmation flow, closing the gap where injected or hallucinated dangerous calls could always only be asked about, never refused.

## ADDED Requirements

### Requirement: Policy denial blocks dispatch before confirmation

The system MUST evaluate configured deny rules against every tool call (tool name and arguments) before the confirmation flow, for both agent-native tools and registry tools. A call matched by a deny rule MUST NOT be dispatched, MUST NOT trigger a confirmation request, and MUST produce a failed tool result visible to the model and the run stream.

#### Scenario: Deny by tool name

- **WHEN** the policy contains a deny rule matching the tool name and the model emits a call to that tool
- **THEN** the tool is not dispatched, no confirmation card is shown, and the tool result is a failure carrying the static policy-denial text

#### Scenario: Deny by argument pattern

- **WHEN** a deny rule matches only the serialized call arguments (e.g. an argument substring or wildcard pattern) and a call's arguments match
- **THEN** the call is blocked exactly as a name-matched denial

#### Scenario: Deny precedes confirmation for mutating tools

- **WHEN** a tool would normally require user confirmation and a deny rule also matches the call
- **THEN** the call is blocked by policy and no confirmation is requested

### Requirement: Rules evaluate first-match-wins

The system MUST evaluate rules in their configured order and decide on the first match. Rules later in the list MUST NOT override an earlier match, and a non-matching rule MUST NOT affect the call.

#### Scenario: Earlier allow rule overrides later deny

- **WHEN** the policy lists an allow rule for a tool before a deny rule that also matches the same call
- **THEN** the call is permitted by the policy layer and proceeds to the existing gates unchanged

### Requirement: Unmatched calls keep today's behavior

When no deny rule matches a call — or no policy is configured at all — the dispatch path MUST be identical to the pre-policy behavior, including the confirmation flow for mutating registry tools and retry handling.

#### Scenario: Policy unset

- **WHEN** no policy source is configured
- **THEN** no policy evaluation occurs and dispatch behavior is unchanged for every call

#### Scenario: Policy set but no rule matches

- **WHEN** a policy is configured and a mutating registry call matches no rule
- **THEN** the call reaches the confirmation flow exactly as it does today

### Requirement: Policy is snapshotted at startup and fails closed

The policy MUST be read and validated once at agent startup. When a policy source is configured but missing, unreadable, or invalid, the agent MUST refuse to start rather than run without the intended policy. Changes to the policy file while the agent is running MUST NOT take effect until restart.

#### Scenario: Invalid policy fails the boot

- **WHEN** the configured policy file contains a malformed or schema-invalid rule set
- **THEN** the agent process exits with a startup error identifying the policy problem

#### Scenario: Mid-run policy edit is inert

- **WHEN** the policy file changes after the agent has started
- **THEN** the running agent continues enforcing the snapshot taken at startup

### Requirement: Denial text is static and safe

The model-facing and wire-facing policy-denial text MUST be static: it MUST NOT embed rule definitions, argument values, file paths, or any other policy or environment detail.

#### Scenario: Denial carries no policy internals

- **WHEN** any call is denied by policy
- **THEN** the denial text is a fixed safe string identical across rules and calls

### Requirement: Policy decisions are audited without content

Every policy decision MUST be recorded via the structured log and a Prometheus counter carrying the tool name, the decision, and the matched rule identifier. Recorded decisions MUST NOT include argument values or any other call content.

#### Scenario: Deny decision is audited

- **WHEN** a call is denied by a rule
- **THEN** the decision log and counter identify the tool, the decision, and the rule, and contain no argument content

### Requirement: Rules cover both tool sources

Deny rules MUST apply uniformly to agent-native tools and registry tools, including runs where the registry is unreachable.

#### Scenario: Agent-native tool denied

- **WHEN** a deny rule matches an agent-native tool call
- **THEN** the call is blocked with the standard policy denial even though agent-native tools never require confirmation
