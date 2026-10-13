# Spec Delta

## Purpose

Exposes agent LLM traffic — prompts, replies, token usage, and step/tool structure — as structured traces in a self-hosted Langfuse instance, so planner and executor behaviour can be tuned from inspection data instead of raw service logs.

## ADDED Requirements

### Requirement: Tracing is opt-in and off by default

The agent MUST NOT construct any trace-export client, or emit any trace payload off the machine, unless `PYTHON_AGENT_LANGFUSE_ENABLED=true` with valid credentials configured. The disabled state MUST be the default in shipped images, env files, and CI test setup. When disabled, no Langfuse connection is attempted and run behaviour is byte-identical to the pre-change agent.

#### Scenario: Default-off in CI

- **WHEN** CI runs the agent test suite without setting `PYTHON_AGENT_LANGFUSE_ENABLED`
- **THEN** no Langfuse client is constructed, no trace payload is produced, and agent runs complete as today

#### Scenario: Disabled flag produces no egress

- **WHEN** the agent runs with the flag unset or false while a Langfuse instance is reachable
- **THEN** the agent makes no request to the Langfuse API for that run

### Requirement: Every model call is captured as a generation

For every LLM invocation the agent makes — planner (structured and legacy), executor `llm` step, executor `react` iterations, and the tool re-prompt — the exported trace MUST contain a generation observation carrying the input messages, the model's output text, the model identifier, and the token usage as reported by the provider (when the provider reports it).

#### Scenario: Planner and executor generations appear

- **WHEN** a run executes with tracing enabled and completes a plan plus at least one `llm` step
- **THEN** the run's trace contains one generation for the planner call and one for the executor call, each with non-empty input and output payloads

#### Scenario: Provider-reported usage is attached

- **WHEN** the model provider returns token usage in its response
- **THEN** the corresponding generation's usage counts reflect the provider-reported values, not the agent's estimated-token constant

#### Scenario: Multi-iteration ReAct calls are each captured

- **WHEN** a `react` step iterates model → tool → model more than once
- **THEN** each model invocation in the loop appears as its own generation under the step's span

### Requirement: Traces carry run and user identity

Every exported trace MUST carry the run's `run_id` (equal to the LangGraph `thread_id`) as the trace session identifier and the verified `X-Nyx-User-Id` as the trace user identifier, so operators can replay a single run end-to-end and filter traces per user.

#### Scenario: Run tree is addressable by session

- **WHEN** a run with `run_id` R completes under user U
- **THEN** querying traces for session R returns exactly that run's trace tree, and the trace's user id is U

#### Scenario: Resumed runs keep their session

- **WHEN** a run resumes an existing `run_id`
- **THEN** the exported trace for the resumed portion carries the same session identifier as the original run_id — not a freshly minted one

### Requirement: Plan steps and tool calls are traced

Each executed plan step MUST appear as a span named after its step kind (`plan`, `rag`, `tool`, `llm`, `react`) carrying the step id and terminal status, and each tool dispatch MUST carry the tool name, its resolved arguments, the outcome (`ok` / `failed`), and the measured duration.

#### Scenario: Tool-step run shows the full structure

- **WHEN** a run executes a plan that includes a tool step which succeeds
- **THEN** the trace shows the plan span, one span per step, and a tool span with the tool name and a non-zero duration attribute

#### Scenario: Failed tool carries its failure

- **WHEN** a tool dispatch fails (registry rejection or exhausted transient retries)
- **THEN** the tool span's status is failed and the enclosing step span reports its terminal `failed` status

### Requirement: Credentials never reach the trace backend

Bearer tokens, JWT-shaped strings, and API-key-shaped values MUST NOT appear in exported trace payloads. Any trace payload the agent constructs itself (step/tool arguments and results) MUST be scrubbed before export, and the agent MUST NOT place the request's `Authorization` header value into prompts, tool arguments, or observation attributes.

#### Scenario: Authenticated tool calls export no bearer

- **WHEN** a run makes tool calls whose dispatch uses the caller's bearer token
- **THEN** no exported span, generation, or metadata contains the token string; credential-shaped substrings in tool args/results are replaced with a redaction marker

### Requirement: Trace delivery is best-effort and never affects runs

Trace export MUST NOT change run outcomes or the SSE stream. When the Langfuse instance is unreachable, slow, or rejects payloads, the agent MUST log at warn level and continue — no exception from trace code may propagate into the run, and the SSE frames for the run MUST remain byte-identical to the tracing-disabled stream.

#### Scenario: Backend unreachable does not fail the run

- **WHEN** a run executes with tracing enabled while the Langfuse instance is down
- **THEN** the run reaches `done` + `terminator` with the same frames as a tracing-disabled run, and a warn-level log records the trace-export failure

#### Scenario: Export does not block the user-visible path

- **WHEN** Langfuse ingestion is slow (payloads queued, not yet acknowledged)
- **THEN** the SSE stream's `done` frame is not delayed by trace flushing

### Requirement: Dev stack provisioning is deterministic

The repository SHALL provide a docker compose profile (`langfuse`) and make targets that bring up the self-hosted stack (`langfuse-web`, `langfuse-worker`, ClickHouse, MinIO, Redis, and a dedicated Postgres). On first boot with the documented `LANGFUSE_INIT_*` variables set, the instance SHALL come up with an organization, project, and API key pair matching the agent's configured credentials — with no UI interaction required. Only the web UI/API port SHALL be published to the host, and it MUST be loopback-bound; all stateful component ports MUST NOT be published.

#### Scenario: Fresh profile up yields a working key pair

- **WHEN** an operator runs the documented up target on a machine with no prior Langfuse volumes
- **THEN** the UI is reachable on `127.0.0.1:3000`, and the agent configured with the documented env values authenticates successfully against the instance without visiting the UI

#### Scenario: Only the UI port is exposed

- **WHEN** the profile is up
- **THEN** the host exposes exactly one Langfuse port (the UI/API on loopback); Postgres, Redis, ClickHouse, MinIO, and the worker are not reachable from the host network
