# Spec Delta

## Purpose

The structured-output binding becomes a per-run decision: when a run carries a user LLM override whose base-URL host is a known-legacy provider (DeepSeek), the planner uses the legacy free-text path for that run regardless of the global flag. The planner also honors the user's configured model.

## MODIFIED Requirements

### Requirement: Plan obtained via provider structured output

The planner SHALL obtain the `Plan` object via a provider structured-output binding (Pydantic-typed response, JSON-schema response format, or provider tool-call wire) and SHALL NOT run `json.loads` on raw free-text model output as part of the primary path. When the binding is disabled by configuration, the planner MAY fall back to the legacy free-text path; the primary path MUST go through the structured binding. The binding's enablement is decided per run: it is disabled when the deployment-wide flag is off, and also when the run's provider selection designates a legacy-only provider for that run, in which case the legacy free-text path is that run's primary path.

#### Scenario: Structured binding returns parsed Plan on first try

- **WHEN** the planner invokes the model with `Plan` as the response schema and the model returns a schema-conformant response
- **THEN** the planner receives a Pydantic `Plan` instance derived from schema-constrained provider output — no fence-stripping and no `json.loads` on unconstrained free-text

#### Scenario: Fence-stripping code is unreachable on the primary path

- **WHEN** the structured binding is enabled for the run — the deployment-wide flag is on and the run's provider selection designates a structured-output-capable provider
- **THEN** no code path on the primary planner flow executes the legacy fence-stripping or `json.loads` logic — that code is either deleted or guarded behind a configuration flag whose default places it on a non-primary branch

## ADDED Requirements

### Requirement: Per-run planner path selection from the user provider

When a run carries an LLM override, the planner SHALL select its emission path per run: an override base-URL host containing `deepseek` SHALL use the legacy free-text path; any other override SHALL follow the global structured-output flag. When no override is present, selection is unchanged (global flag only). The bounded self-check retry SHALL apply identically on both paths.

#### Scenario: DeepSeek override runs the legacy path

- **WHEN** a run's override base URL host contains `deepseek` and the global structured flag is enabled
- **THEN** the planner for that run uses the legacy free-text emission and parse path, without re-reading or mutating global configuration

#### Scenario: Non-DeepSeek override keeps the structured path

- **WHEN** a run's override base URL points at a provider whose host does not contain `deepseek`
- **THEN** the planner for that run uses the structured binding when the global flag is enabled

#### Scenario: No override keeps today's selection

- **WHEN** a run carries no override (direct or test use)
- **THEN** planner path selection is byte-identical to the pre-change behavior

### Requirement: Planner honors the run's configured model

When a run carries an LLM override, the planner SHALL construct its client with the override's model rather than the deployment's planner-model configuration, so the user's configured model governs plan emission as well as execution. When no override is present, the planner model is unchanged.

#### Scenario: Override model applies to plan emission

- **WHEN** a run's override specifies a model
- **THEN** the planner's client is constructed with that model, verified at every planner construction site
