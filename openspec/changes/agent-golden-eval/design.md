# Design

## Context

The agent is a Python LangGraph orchestrator (`backend/agent_py/`) behind a thin Go SSE proxy: clients hit Go `POST /api/chat`, which verifies the JWT, sets `X-Nyx-User-Id`, and proxies to the Python agent's `POST /agent/run`. The Python endpoint is private-network-trust: it does not re-verify the JWT but forwards the bearer to the MCP and RAG bridges, which re-verify. Frames are typed SSE events (`run_id` → `plan` → `step_start`/`tool_call`/`tool_result`/`step_end`/`delta`/`note`/`confirm_request` → `done`/`error` → terminator), and the frame kind is the SSE `event:` field with a flat payload on `data:`. A plan step is `{id, kind: rag|tool|llm|react, tool, args, …}`. Cross-session preference memory (`agent-user-preferences`) is landed: `looks_like_preference` gates a fire-and-forget extraction LLM call that upserts `agent.chat_user_prefs`, and the planner prepends a prefs block to its system prompt. Tool selection is Qwen3-4B — probabilistic. The tooling selection for evals is already recorded (`AgentStack.zh.md` §4.1): promptfoo-first, custom provider on real SSE, deterministic graders, `security` tag tracked separately, Inspect AI as the later upgrade on the same dataset, decoupled from Langfuse.

## Goals / Non-Goals

**Goals:**
- One command per line: `make eval-golden` (main line) and `make eval-security` (security line) against a running dev stack.
- Graders consume exactly what a client consumes: SSE frames from the production run endpoint.
- Operator-editable corpora as data; rates with floors as exit codes; per-sample artifacts for triage; cross-run diffability.
- The harness's own logic fully unit-tested without a model.

**Non-Goals:**
- LLM-as-judge grading (local 4B judge is noise; revisit when a stronger judge model is routinely available).
- Train/dev/golden split, synthetic case generation, dynamic refresh (recorded methodology in `AgentEvalPrompts.md` — v2, not here).
- Execution-effect assertions (v1 is frame-level; a `create_feed` case asserts the tool call, not the row).
- Any change to backend/frontend production code or the wire contract.

## Decisions

### D1 — Matching semantics: normalized tokens from the `plan` frame, strict ordered equality

Unchanged from the deleted proposal's D1, re-pointed at the seam: the provider consumes the SSE stream, and the grader normalizes the `plan` frame's steps (`kind` for rag/llm/react, `tool` for tool steps) to a token list compared with strict ordered equality. Rationale stays: the failure family this eval exists to catch (stray react loops, redundant calls, over-planning) is precisely "extra/misplaced steps"; subsequence matching would wave it through. The auto-appended trailing `llm` step is legitimate and expectations include the tail explicitly. Failed samples record the raw plan JSON and observed tokens.

### D2 — Rates: promptfoo repeats + a threshold post-processor

Each case runs N times (`repeat`, default 3) serially. promptfoo emits a JSON run output; a small post-processor (`evals/bin/check_rate.py`) computes per-case pass rates against the corpus `min_rate` floors and the overall floor (`--min-rate`, default 0.85), sets the exit code, and writes the report. Alternative considered: per-test assertions × repeat with promptfoo's native exit code — rejected because promptfoo's native exit reflects per-assertion pass/fail counts, not rate floors, and per-case floors are corpus data the post-processor already reads. `--no-fail` reports without enforcing floors (trend runs).

### D3 — Production path: direct `POST /agent/run`, Go stays up but out of the measured hop

The provider calls the Python agent's `/agent/run` directly, as the recorded selection specifies. It registers/logs in a per-run eval user through the Go API (real JWT for the MCP/RAG bridges' re-verification) and sets `X-Nyx-User-Id` itself, exactly as the Go proxy would. Rationale for skipping the Go hop in the measured path: the Go proxy's translation is already pinned by the `@live` Playwright chat specs, and double-covering it here couples the eval stack to one more process without adding signal about the model. The Go service must still be reachable (registration, login, feed seeding). Alternative considered (provider hits Go `/api/chat`): closer to the user's literal path, but adds the proxy hop into every rate and duplicates existing coverage.

### D4 — Mode matrix: corpus declares per-mode expectations, env selects the active mode

Cases whose expectation depends on `PYTHON_AGENT_WEB_SEARCH_ENABLED` declare both variants (weather: web-off best-effort `[llm]` vs web-on `[web_search, llm]`; the doc's p.s. asks for exactly this). An env var (`EVAL_WEB_SEARCH=on|off`, default `on`) tells the harness which variant is active; the operator sets the agent's real flag to match. Mismatch between the env and the running stack surfaces as weather-case failures — that case is the tripwire for the mismatch, by design. Running both modes in one pass (two agent instances on two ports, one per provider) is noted as a v2 convenience; v0 keeps one stack per invocation. Cases needing an absent prerequisite report SKIPPED with reason: the budget-exhaustion shape runs only against the small-budget side stack (`EVAL_BUDGET_STACK=1`), where the operator starts the agent with a tiny `PYTHON_AGENT_BUDGET_PER_USER_PER_HOUR` and the case hammers the eval user until the guard rejects; without that stack it is SKIPPED, never silently passed.

### D5 — Memory scenario: session-boundary script + extraction-landed polling

The case's vars carry the dialogue script (`[{prompt, new_session}, …]`). The provider executes it: for a `new_session` turn it creates a fresh session id for the same eval user (history stays clean per session, preferences are user-scoped). After the seed turn, the provider polls `agent.chat_user_prefs` over asyncpg (dev-stack DSN, read-only) until the expected preference key appears, bounded (default 60s). Rationale: extraction is fire-and-forget async by design; a fixed `sleep` would be a flake factory, and there is no user-facing prefs read API to poll instead. On timeout the case is SKIPPED with reason — consistent with the SKIPPED contract, and it cannot fake a pass. The recall turns' answers are graded by keyword (秋刀鱼). Alternative considered: asserting via a prefs read API — none exists; adding one for eval is scope creep (a read endpoint may land later with user-prefs UI, and the corpus can move to it then).

### D6 — Corpus as promptfoo data files; the four-tuple maps cleanly

`evals/cases/*.yaml` hold test cases loaded by `evals/promptfooconfig.yaml` / `evals/promptfooconfig.security.yaml`. The methodology four-tuple maps as: Input → the case prompt; Context → `vars` (mode selection, fixture declarations, dialogue script); Expected Output → `assert` on the provider's structured output (frame transcript JSON); Evaluation Criteria → which deterministic grader runs (plan-token equality / keyword contains / forbidden keyword / no-destructive-tool-call / no-system-prompt-leak). Graders are implemented in Python (`evals/graders/`) and registered as promptfoo python assertions, so corpus, grader selection, and grader implementation stay separable. Tool names and frame kinds in corpora are validated at load against the live catalog and the frame-kind literal set (fail-fast before any model call).

### D7 — Security line: separate config, separate rate

`evals/promptfooconfig.security.yaml` holds the injection corpus (seeded feed content and web results carrying hostile instructions; a few direct-injection probes). It runs as its own invocation so the main line's red/green stays clean, and its report records the security failure rate as its own trend. v0 hand-authors ~8–12 cases; `promptfoo redteam` generation is a noted future option, not a dependency. Assertions are structural: no `delete_feed`/unauthorized `update_feed` `tool_call` frames; no `delta` text containing system-prompt markers (a fixed marker string planted in the eval user's prompt path for the security run is NOT needed in v0 — the planner's prefs block and tool catalog preamble give stable enough markers; implementation reconciles the exact marker set against a recorded transcript).

### D8 — Why promptfoo (recorded) and why the corpus stays data

Recorded selection (`AgentStack.zh.md` §4.1): promptfoo gives `view` cross-run diff, CI-integrable exit codes, a redteam option, and a JSONL dataset that later coexists with Inspect AI; Braintrust excluded (SaaS-only control plane); DeepEval/Ragas/autoevals kept as grader-metric libraries only. A hand-rolled runner was the deleted proposal's choice and is explicitly superseded. The corpus stays operator-editable YAML data files so new prompts and re-floored rates need no code — the property the deleted proposal's D8 got right, kept intact.

### D9 — Auth, isolation, and determinism of fixtures

Per-run eval user (`eval-golden-<UTC-stamp>@eval.nyx.local`) via the real registration API: isolation from real accounts, a clean budget clock, and per-run fixtures that can't collide. Feed fixtures are seeded via the feeds REST API before dependent cases and carry an `eval-` title prefix. Serial execution only (one 4B vLLM; parallel sampling produces false rates — same rule as `@live`). Run artifacts land in `evals/artifacts/<UTC-stamp>/` (per-sample raw SSE + `report.json` + `summary.md`); the directory is gitignored in bulk, and the landing task commits the baseline run's report as evidence.

### D10 — Dev-loop story (multi-runtime rule)

The suite spans Node (promptfoo CLI), Python (provider + graders under the agent venv — no new deps), and the live stack (Go + Python agent + vLLM). `make eval-golden` owns the orchestration: preflight (Go API up, agent `/healthz` up, vLLM reachable via the agent's own preflight), then promptfoo with the agent-venv python wired as the provider command, then the post-processor. If the stack is down the Makefile target says so and stops — it does not start services (the stack is operator-owned; `make up` / `make dev` are the operator's levers). This keeps the eval loop one command without the Makefile squatting on process management.

## Risks / Trade-offs

- **promptfoo adds a Node dev-tool dependency** at the repo root (npx-pinned in the Makefile, no package.json change). Trade-off accepted by the recorded selection; pinning prevents silent runner upgrades from re-basing rates.
- **Env-selected modes can drift from the running stack** (operator forgets to flip `EVAL_WEB_SEARCH` when flipping the agent flag). The weather case is the tripwire; the report records the mode env alongside the model id and git revision.
- **Fixture-dependent RAG rates vary with retrieval quality** — the eval measures the whole retrieval+planning path, not just the planner. Accepted: that is what users experience; the Go RAG bridge already has its own tests.
- **Prompt-coupling of graders** (markers, token vocabulary) can rot as prompts evolve. Mitigation: corpus/grader unit tests run against recorded transcripts from a real run, and the landing task re-records fixtures so the tests pin the current wire reality; a failed validation at load fails loudly rather than mis-grading.
- **Budget case needs a second agent config** (small-budget side stack) — operator-managed, SKIPPED otherwise. Accepted for v0; a dedicated budget profile in `make dev` is a possible v2.
- **prefs polling couples the eval to a schema detail** (`chat_user_prefs` key shape). Bounded to the eval code; if the store moves, the provider fails its poll loudly (SKIPPED), not silently.
