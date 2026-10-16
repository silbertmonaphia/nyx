# Proposal

## Why

The planner is a probabilistic model (Qwen3-4B): prompt regressions, wrong tool selection, over-planning, and now memory-injection misses all pass every deterministic test layer while showing up as *rates* in live behavior. `AgentEvalPrompts.md` (operator-authored) has since grown past the corpus the deleted first proposal covered — it now demands a cross-session memory eval (the 秋刀鱼 case mirroring bugs.md #1) and records the industry golden-set methodology — and `AgentStack.zh.md` §四 item 1 has since pinned the tooling selection (promptfoo-first, Inspect AI upgrade path, deterministic graders, `security` tag for injection). The deleted proposal drifted from both: it specced a hand-rolled Python harness with in-process planner calls, no memory scenario, no security line. This change re-proposes the eval suite on the recorded selection.

## What Changes

- New top-level `evals/` directory — a promptfoo suite, not a hand-rolled harness:
  - `evals/promptfooconfig.yaml` — the main regression line: v0 carries the operator doc's seven cases (rag-only, feed-tool create/update/delete, weather ×2 modes, forced web search, combined multi-step) plus refusal, budget-unavailable, and the memory dialogue — ~10 cases over five of the recorded selection's six shapes. Growing the corpus to the recorded 30–50-case / six-shape target (react case first) is v0.1, per Non-goals. Corpora are authored as operator-editable YAML data files.
  - `evals/promptfooconfig.security.yaml` — the `security` line, failure rate tracked separately from the main line: indirect injection (hostile instructions smuggled through seeded feed content / web results) asserted to NOT execute destructive tools (`delete_feed`…) and NOT leak system-prompt text into `delta` frames; a small set of direct-injection probes. Hand-authored v0 set; promptfoo redteam generation noted as a future option.
  - `evals/cases/*.yaml` — case corpora (Input / context vars / expected / criteria per case), loaded by the configs.
- Custom promptfoo provider (`evals/providers/agent_run.py`, Python, run under the agent venv): registers/logs in a dedicated eval user via the Go API, seeds that user's feeds when a case declares fixtures, then calls the **production chat path for real** — `POST /agent/run` on the Python agent, consuming the true SSE stream (`run_id` → `plan` → `step_*`/`tool_*`/`delta` → `done`/`error`/`confirm_request`). The provider answers every `confirm_request` (mutating calls are gated fail-closed and nothing auto-approves) by auto-approving via `POST /agent/approvals`, so feed-mutation cases execute and the security line's "no destructive `tool_call`" assertion measures the planner rather than the gate. The full frame transcript is the provider output (graders assert against structured frames); raw SSE is also dumped per case to `evals/artifacts/<run>/` — the triage surface per the recorded observability layering (Langfuse is the microscope, not a dependency).
- Deterministic graders only (the recorded decision: a local 4B judge is noise, LLM-as-judge stays out of v0):
  - Plan-shape assertions: normalized token sequence (`step.kind`, or `step.tool` for tool steps) extracted from the `plan` frame, strict ordered equality — the matching semantics of the deleted proposal, re-pointed from the in-process plan object to the wire frames.
  - Answer-content assertions: keyword/phrase checks on the answer text assembled from the run's `delta` frames (`done` carries usage only — no text); the memory scenario asserts 秋刀鱼.
  - Structural frame assertions: no destructive `tool_call` frames, no `delta` text containing system-prompt markers.
- Memory scenario (`agent-user-preferences` acceptance): one case = a scripted multi-session dialogue (vars-driven turns with explicit new-session boundaries: seed 「我最近爱吃秋刀鱼」 → new session 「我喜欢吃什么」 → new session 「推荐吃的给我」). The provider advances sessions only after the seed turn's fire-and-forget extraction has landed, polled via `agent.chat_user_prefs` over asyncpg (bounded wait; timeout → SKIPPED with reason, never a silent pass).
- Mode matrix: cases whose expectation depends on `PYTHON_AGENT_WEB_SEARCH_ENABLED` declare per-mode expectations (weather case carries both); the active mode is selected by an env var at run time, matching the running stack. Cases whose prerequisites are absent (budget-exhaustion case needs the small-budget side stack, started through the established `scripts/e2e-side-stack.sh` convention with `AGENT_BUDGET_PER_USER_PER_HOUR` overridden) are reported SKIPPED with the reason. The web-injection security cases are served by a local Tavily stub the suite ships (`PYTHON_AGENT_TAVILY_BASE_URL` points at it on the eval stack) — hostile web results are not reproducible against the real search API.
- `make eval-golden` / `make eval-security` — pinned promptfoo via npx, run post-processor computing per-case rates over promptfoo `--repeat N` samples and failing on floor breaches (per-case `min_rate` in the corpus; overall floor flag; `--no-fail` for trend runs). Serial only — one 4B vLLM.
- Live-gated, operator-clocked (same posture as `@live` e2e): never in default CI; the deterministic graders/corpus validation/provider SSE parsing are covered by ordinary unit tests, which parse **recorded frame fixtures** from a real run — the grader is the consumer, round-tripping the producer's actual bytes (repo wire-contract rule).
- `AgentEvalPrompts.md` is reduced to the human narrative (why each case exists, rate-not-verdict philosophy) plus a pointer to the corpora — the operator confirms this edit; the operator-authored methodology section stays as the v2 roadmap (train/dev split, LLM-as-judge when a stronger judge model is available, synthetic expansion, dynamic refresh are explicitly non-goals here).
- Artifacts: `evals/artifacts/` and promptfoo's local run store are gitignored in bulk; the baseline report from the landing task is committed as evidence.

## Capabilities

### New Capabilities

- `agent-golden-eval`: the corpus schema (case × mode matrix, expectations pinned to production tool names and wire frame kinds), the provider contract (real `POST /agent/run` SSE, full-frame transcript, seeded fixtures, SKIPPED-with-reason semantics), the grading semantics (plan-token strict equality from `plan` frames, keyword assertions on `done` text, structural security assertions, rate-over-N-samples with floors), the memory scenario protocol (session boundaries, extraction-landed polling), and the artifact/report contract.

### Modified Capabilities

- None.

## Impact

- **Code**: `evals/` (new — configs, corpora, provider, graders, post-processor), root `Makefile` (two targets), root `.gitignore` (artifact bulk-ignore). No backend or frontend code changes; no wire changes.
- **Dependencies**: `promptfoo` (dev tool via pinned npx — Node-side only, not a frontend dependency); Python provider reuses the agent venv's existing httpx/pydantic/asyncpg — no new Python deps.
- **Docs**: `AgentEvalPrompts.md` (pointer edit, operator-confirmed), `backend/agent_py/README.md` (how to run, prereqs, artifact location), `AgentStack.zh.md` §4.1 status line tick.
- **Tests**: deterministic unit tests for graders + corpus validation + provider SSE parsing against recorded fixtures; the live corpus run itself is operator-executed and its baseline committed.
- **Rollout gates**: none — dev-only tooling with no production flip and no deployed-stack gates, so per the classification test in `openspec/config.yaml` there is no `-rollout` sibling.

**Non-goals (v0)**: growing the corpus to the recorded 30–50-case / six-shape target (the `react` shape and corpus expansion land as v0.1 on the same harness); execution-effect assertions (that `create_feed` wrote a row); LLM-as-judge grading; train/dev/golden split and synthetic case generation (recorded in `AgentEvalPrompts.md` as methodology to adopt in v2, not here); automatic prompt tuning; replacing the `@live` Playwright specs (the planner-journey checks there stay).
