# Design

## D1 — Matching semantics: normalized tokens, strict ordered equality

A plan's steps (structured planner: `{id, kind: rag|tool|llm|react, tool, args, prompt, query, max_iterations}`) normalize to a token list: `step.kind` for `rag` / `llm` / `react`; `step.tool` for `kind == "tool"`. Expectations are written in those tokens (`[rag, llm, create_feed]`, `[web_search, llm]`).

Verdict per sample: token list must EQUAL the expected list exactly — no extra steps, no missing steps, order significant. Rationale: the failure family this eval exists to catch (Bug 1's stray react loop, redundant tool calls, over-planning) is precisely "extra/misplaced steps"; lenient subsequence matching would let all of it through. The planner's auto-appended trailing `llm` step (when the model omits it) is legitimate — expectations include the `llm` tail explicitly, so both the model-emitted and auto-appended forms pass identically.

Per-sample failures record the raw plan JSON so triage sees what the model planned, not just that it failed.

## D2 — Rates, not verdicts

A case × mode result is a rate over N samples (default 3). A case passes its mode when `rate >= min_rate` (per-case floor in the YAML; default set after the baseline run, initial `1.0` for the deterministic-looking cases, lower for the weather web-off best-effort case). The run exits non-zero when any case breaches its floor or the overall rate is below `--min-rate` (default 0.85); `--no-fail` reports without the exit code for trend watching. Report includes per-sample detail so a 1/3 case is triageable.

Sampling parameters are the production ones (planner settings as configured for the live service) — the eval measures the system the users get, not a tuned copy.

## D3 — Live-gated, never in CI

The runner needs a reachable model stack (the operator's vLLM + the existing `LLM_*` settings). It preflights with a tiny completion and exits with a clear message when unreachable. It is not part of `pytest`'s default collection (it is a CLI runner, not a test module) and therefore cannot redden CI; the deterministic harness logic is covered by normal unit tests instead. This mirrors the `@live` e2e split: nondeterministic, environment-hungry checks run against a live stack on an operator clock.

Serial execution only (one 4B vLLM instance; parallel sampling produces false rates).

## D4 — Same path as production planning

The runner imports and calls the same function the graph's `plan_node` uses to produce a plan — it never re-implements prompt assembly or plan parsing. A unit test pins this (the runner's call target is the one `plan_node` invokes), following the repo's wire-contract rule: the harness must consume the producer, not a copy of it.

## D5 — Config matrix via Settings construction

Mode flags (`web_search_enabled`) are properties of the `Settings` instance the planner is called with. The runner constructs a `Settings` per mode in-process (not by re-launching the service), so the matrix runs in one pass. Catalog inclusion of `web_search` may also require a configured search key; a mode whose prerequisites are absent (e.g. web-on without a key) is reported as SKIPPED with the reason, never silently passed.

## D6 — Corpus schema (source of truth)

```yaml
cases:
  - id: feed-from-summary
    prompt: "总结最近3个月我发的feed，形成一篇新feed发出去"
    modes:
      - when: { web_search: false }
        expect: { tokens: [rag, llm, create_feed] }
        min_rate: 1.0
  - id: weather
    prompt: "今天天气如何？"
    modes:
      - when: { web_search: false }
        expect: { tokens: [llm] }        # best-effort; floor set from baseline
        min_rate: 0.6
      - when: { web_search: true }
        expect: { tokens: [web_search, llm] }
        min_rate: 1.0
```

`AgentEvalPrompts.md` keeps the narrative (why each case exists, the rate-not-pass/fail philosophy) and points at the YAML; the YAML is what the runner reads. Expected token names are pinned against the live catalog during implementation (the doc's `add_feed`/`create_feed` mismatch is one such fix).

## D7 — Reports

`eval/reports/<UTC-stamp>/report.json` (stable schema: `cases[].modes[].samples[].{plan, tokens, verdict, error}`, rates, `meta: {model, git_rev, started_at, samples}`) plus `summary.md` (the table a human reads: per-case rate, failing sample tokens, report paths). Reports are gitignorable bulk; the BASELINE report from task 5 is committed as evidence.

## D8 — Why new dev tooling, not a test file

The corpus is data an operator edits (new prompts, tuned floors) without touching Python, and the verdict is a rate table, not a test assertion. A pytest module would fight both (collection semantics, exit-code meaning, YAML-in-test-parameters). The harness is packaged as `eval/` with deterministic unit tests around it, exactly the split the doc's closing note describes.
