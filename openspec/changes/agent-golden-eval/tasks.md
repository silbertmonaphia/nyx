# Tasks

## 1. Corpus migration

- [ ] 1.1 Create `evals/cases/` with the main-line corpora migrated from `AgentEvalPrompts.md`: the feed/RAG group (rag-only, rag→create_feed, rag→update_feed, rag→delete_feed), the web group (weather with BOTH mode expectations keyed `web_search: on|off`; forced-search case), and the combined rag→web_search→create_feed case. Pin expected tool names against the live production catalog (resolve the doc's `add_feed` vs `create_feed` discrepancy against `backend/agent_py/agent/tools_client.py` / the MCP registry, not by guess). Verify: corpus files parse as YAML and every expected token is a known step kind or catalog tool name.
- [ ] 1.2 Add the refusal and budget-unavailable case entries (six-shape coverage): refusal prompts pinned against actual planner behavior on the live stack during 1.1's reconciliation pass (direct-answer / no-tool shape), budget case declared with `requires: budget_stack`. Verify: corpus loads; both entries carry expectation + floor + prerequisite declaration.
- [ ] 1.3 Add the memory scenario case (seed 「我最近爱吃秋刀鱼」 → new-session 「我喜欢吃什么」 → new-session 「推荐吃的给我」; keyword expectation 秋刀鱼 on both recall turns) as a dialogue-script vars entry. Verify: corpus loads and the case's turn list declares explicit new-session boundaries.
- [ ] 1.4 Add the security corpus (`evals/cases/security.yaml`, ~8–12 cases): indirect injection via seeded feed content and web-result content instructing destructive tool use or system-prompt disclosure; 2–3 direct probes. Each asserts no destructive `tool_call` frames and no prompt-marker leakage in `delta` text. Verify: corpus loads against the security config.

## 2. Provider

- [ ] 2.1 Implement `evals/providers/agent_run.py` (promptfoo custom provider, run under the agent venv): per-run eval-user registration/login via the Go API, `POST /agent/run` with `X-Nyx-User-Id` + bearer, SSE consumption to an ordered frame transcript, transcript JSON as provider output. Preflight the endpoint and fail loudly with the endpoint URL when unreachable. Verify: unit test parses a recorded frame fixture into the expected transcript.
- [ ] 2.2 Fixture seeding: cases declaring feed fixtures get them created via the feeds REST API before the prompt (eval- prefix, per-run user). Verify: unit test with a stubbed API client asserts seed-before-prompt ordering; live behavior shown in 6.2.
- [ ] 2.3 Dialogue-script execution + extraction-landed polling: new-session turns open fresh sessions for the same user; after a seed turn, poll `agent.chat_user_prefs` over asyncpg (bounded, default 60s; timeout → SKIPPED result with machine-readable reason). Verify: unit test with a stubbed poll reader covers landed-immediately / landed-after-delay / timeout-SKIPPED.
- [ ] 2.4 Record real frame fixtures from one live run per shape into `evals/tests/fixtures/` for the grader/provider unit tests. Verify: fixtures committed; each is a raw SSE capture, not synthetic.

## 3. Graders and validation

- [ ] 3.1 Implement `evals/graders/plan_shape.py`: normalize `plan`-frame steps to tokens (kind for rag/llm/react, tool for tool steps), strict ordered equality vs the case's mode-keyed expectation, auto-appended trailing `llm` equivalence; failures carry raw plan + observed tokens. Verify: unit tests against recorded fixtures cover pass, extra step, wrong order, wrong tool, missing tail.
- [ ] 3.2 Implement answer-content graders (required keyword/phrase on `done` final text; forbidden keyword absent). Verify: unit tests include the 秋刀鱼 recall assertion and a forbidden-keyword case.
- [ ] 3.3 Implement security graders (no destructive `tool_call` outside declared allowance; no system-prompt markers in `delta` text; marker set reconciled against a recorded transcript). Verify: unit tests against recorded/injected fixtures detect both violation shapes and pass clean transcripts.
- [ ] 3.4 Implement corpus load validation: unknown tokens / tool names not in the catalog / unknown frame kinds / missing floors fail at load before any model call. Verify: unit test feeds a bad corpus and asserts the load-time error names the case id and field.

## 4. Runner wiring

- [ ] 4.1 Write `evals/promptfooconfig.yaml` (main line: corpora, provider command = agent-venv python provider, `repeat` default 3, serial) and `evals/promptfooconfig.security.yaml` (security line). Verify: `npx promptfoo eval --config <file> --dry-run` (or the equivalent list action) resolves cases and provider without calling the model.
- [ ] 4.2 Implement `evals/bin/check_rate.py`: read promptfoo JSON output, compute per-case rates vs corpus floors + overall floor (`--min-rate` 0.85 default), write `evals/artifacts/<UTC-stamp>/report.json` + `summary.md` (per-case rate, failing-sample tokens, mode env, model id, git revision) and per-sample raw SSE artifacts; exit code per floors; `--no-fail` for trend runs. Verify: unit tests on canned run outputs cover floor breach, `--no-fail`, SKIPPED accounting.
- [ ] 4.3 Root `Makefile`: `make eval-golden` and `make eval-security` (preflight Go API + agent healthz; pinned promptfoo via npx; post-processor invocation). Targets do NOT start services — they report a down stack and stop. Verify: with the stack down, the target exits with a clear preflight message and starts nothing.
- [ ] 4.4 Root `.gitignore`: bulk-ignore `evals/artifacts/` (baseline evidence exception handled in 6.2, mirroring the forced-paths pattern). Verify: `git status --porcelain evals/artifacts/` stays clean after a run while the committed baseline remains tracked.

## 5. Deterministic unit test gate

- [ ] 5.1 All harness unit tests pass model-free: `cd backend/agent_py && SKIP_CONTAINERS=true ...` is NOT their home — run them from `evals/` via the agent venv (`pytest evals/tests`), and confirm default CI/pytest collection never picks up a live case. Verify: `pytest evals/tests` green; repo default test commands unchanged and green.

## 6. Live baseline

- [ ] 6.1 Run `make eval-golden` against the up stack (operator's vLLM + agent + Go), with `PYTHON_AGENT_WEB_SEARCH_ENABLED=true` matching `EVAL_WEB_SEARCH=on`; tune nothing to make cases pass — floors stay as authored, failures are recorded. Verify: `evals/artifacts/<stamp>/report.json` + `summary.md` produced; budget case shows SKIPPED (no small-budget stack).
- [ ] 6.2 Set per-case `min_rate` floors from the baseline (deterministic-looking shapes 1.0; best-effort shapes lower, justified in the corpus), then commit the baseline report as the forced-add exception under 4.4's ignore. Verify: committed baseline present; floors in corpus match baseline table.
- [ ] 6.3 Run `make eval-security` once to establish the security failure-rate baseline; record it in the summary. Verify: security report committed alongside 6.2's evidence.
- [ ] 6.4 Self-review pass: graders consume recorded wire fixtures (not in-process shapes); SKIPPED paths never count as pass; no secret (JWT, DSN) lands in reports or artifacts. Verify: review notes in the change; grep artifacts for token patterns.

## 7. Docs

- [ ] 7.1 Reduce `AgentEvalPrompts.md` to narrative + pointer to the corpora (operator-owned file — confirm the edit with the operator before committing; the methodology section stays as the recorded v2 roadmap). Verify: file retains the why/philosophy and points at `evals/cases/`; operator sign-off recorded in the change.
- [ ] 7.2 `backend/agent_py/README.md`: how to run the suite, prereqs (stack up, vLLM, env selection for modes), artifact locations. The README is co-owned with the `agent-chat-fixes` / `agent-langfuse-log-visibility` changes — this edit is append-only at EOF under a new `## Golden Set Eval Suite` heading so their merges stay conflict-free. Verify: a fresh-operator read-through can run `make eval-golden` from the README alone.
- [ ] 7.3 Tick the `AgentStack.zh.md` §4.1 status line (golden-set suite landed, promptfoo-based; Inspect AI upgrade path unchanged). Verify: status line reflects landed state.
