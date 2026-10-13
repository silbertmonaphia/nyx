# Proposal: agent-langfuse-tracing

## Why

The agent's planner/executor LLM traffic is invisible. Today the Python agent emits OTel spans
(metadata only) and Prometheus counters (token totals), but per-call prompts, model replies,
reasoning, and usage data are never persisted — `AgentStack.zh.md` §三-A records this as gap A and
ranks **Langfuse 自托管 as the #1 queued item by ROI**: "planner 调优只能肉眼读 log". The
LangGraph-native integration seam (callbacks in the graph config) makes this the cheapest point in
the stack to close the gap, and self-hosting keeps prompts (which embed user-derived content) on
the machine — consistent with the local-only observability posture (Jaeger, best-effort local
cache).

## What Changes

- **Self-hosted Langfuse stack** as an opt-in docker compose profile (`langfuse`): Langfuse v4
  `langfuse-web` + `langfuse-worker` + dedicated ClickHouse / MinIO / Redis / Postgres. Only the
  UI port is published, loopback-bound; all stateful internals stay on the compose network.
  `LANGFUSE_INIT_*` bootstrap provisions org/project/API keys on first boot so the dev loop needs
  no UI clicks. New `make up-langfuse` / `make down-langfuse` targets; `make status` reports it.
- **Opt-in LLM tracing in agent_py** via `PYTHON_AGENT_LANGFUSE_ENABLED` (default `false`). When
  enabled, a Langfuse LangChain `CallbackHandler` rides the existing graph-invocation config
  (`graph_config`) so every ChatOpenAI call site — planner (structured + legacy), executor `llm`,
  executor `react`, tool re-prompt — becomes a Langfuse generation carrying input messages, output
  text, model name, and real token usage (which the agent currently discards; `DoneEvent` stays
  zero-valued — see out of scope).
- **Plan steps and tool calls traced** as explicit Langfuse observations: step kinds (`plan`,
  `rag`, `tool`, `llm`, `react`) and MCP tool dispatch get spans with name, args, result status,
  and duration — the pieces the CallbackHandler cannot see because tools dispatch via raw MCP
  JSON-RPC, not LangChain tools.
- **Identity mapping**: Langfuse `session_id` = `run_id` (= `thread_id`), `user_id` = the
  Go-verified `X-Nyx-User-Id` — so the UI filters per user and replays per run.
- **Safety posture**: credential-shaped strings (bearer tokens, API keys) masked before export;
  tracing is best-effort — a Langfuse outage drops traces with a `log.Warn` and never fails or
  slows a run; `LANGFUSE_ENABLED=true` without keys refuses startup (house refusal pattern).
- **Docs/roadmap**: strike queued item 1 in `AgentStack.zh.md` §四 + update the §三-A row; flip
  the "Langfuse 式 prompt/response 级 trace 未做" note in `AgenticStackNaive.zh.md` P1.3;
  document the new env keys + stack in `backend/agent_py/README.md`.

Not changing: the SSE wire contract (frames byte-identical), the Go backend, frontend, Jaeger
OTel pipeline, or Prometheus counters.

## Capabilities

### New Capabilities

- `agent-llm-tracing`: LLM-level trace export from the Python agent to a self-hosted Langfuse
  instance — opt-in gating and fail-open delivery, generation capture (prompts / replies / usage),
  step- and tool-level observations, run/user identity mapping, credential masking, and the
  dev-stack provisioning contract (compose profile + deterministic key bootstrap).

### Modified Capabilities

- `agent-python-service`: the "Configurable via environment" requirement gains the
  `PYTHON_AGENT_LANGFUSE_*` keys and their refusal semantics (enabled-without-keys refuses
  startup).

## Impact

- `backend/agent_py/`: `config.py` (new `PYTHON_AGENT_LANGFUSE_*` settings + validator),
  `observability.py` (Langfuse client setup, masking, step/tool observation helpers), `graph.py` /
  `main.py` (handler attach at the `graph_config` seam, identity metadata), `executor.py` /
  `planner.py` (step + tool observation wrapping), `pyproject.toml` (new `langfuse` dependency),
  tests + e2e.
- `docker-compose.yml` (new `langfuse` profile services), `Makefile` (`up-langfuse`,
  `down-langfuse`, `status`), root `.env.example` (Langfuse compose secrets),
  `backend/agent_py/.env.example` (+ compose `agent` service env overrides), agent README.
- No Go / frontend / SSE / DB-schema changes. No new backend runtime dependency.
- Known context: the production `run_agent_stream` path emits no `agent.run` OTel span today
  (spec/code drift vs `agent-observability`); noted in design, deliberately not fixed here.
- Rollout gates tracked in `openspec/changes/agent-langfuse-tracing-rollout/`.
