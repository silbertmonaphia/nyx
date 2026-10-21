# Baseline

## Run

- stamp: `20261021T174740Z`
- artifacts: `evals/artifacts/20261021T174740Z/`
- git revision: `c482adce8054c010c56513a9639cf73e4fea9713` (pre-implementation HEAD)
- model: `Qwen/Qwen3-4B-Instruct-2507-FP8`
- mode: `web_search=on`, `budget_stack=off`
- stack: isolated side stack (Go `:8182`, agent `:9102`, Vite `:5274`), host vLLM `:8000` + embed `:8001`
- duration: 7m 12s, serial
- overall: **0.73 (24/33 graded, 3 skipped)**; per-case floors breached on three cases
- overall floor: **0.7** on this run (Makefile default). The 2026-10-16 artifact recorded `0.85`; both runs breach the same three case-level floors. Floor provenance is task 7.1.

## Cases

| case | rate | pass | fail | skip | floor | verdict |
| --- | --- | --- | --- | --- | --- | --- |
| main-rag-summary-recent | 1.00 | 3 | 0 | 0 | 0.66 | PASS |
| main-rag-create-feed | 0.00 | 0 | 3 | 0 | 1.0 | BREACH |
| main-rag-update-feed | 1.00 | 3 | 0 | 0 | 1.0 | PASS (hollow) |
| main-rag-delete-feed | 1.00 | 3 | 0 | 0 | 1.0 | PASS (hollow) |
| main-web-weather-modes | 1.00 | 3 | 0 | 0 | 0.66 | PASS |
| main-web-force-search | 0.00 | 0 | 3 | 0 | 1.0 | BREACH |
| main-all-tools-combined | 0.00 | 0 | 3 | 0 | 0.66 | BREACH |
| main-refusal-direct-answer | 1.00 | 3 | 0 | 0 | 1.0 | PASS |
| main-smalltalk-greeting | 1.00 | 3 | 0 | 0 | 1.0 | PASS |
| main-rag-feed-recall | 1.00 | 3 | 0 | 0 | 0.66 | PASS |
| main-budget-exceeded | — | 0 | 0 | 3 | 1.0 | SKIPPED |
| main-memory-preference-recall | 1.00 | 3 | 0 | 0 | 0.66 | PASS |

## Fresh evidence per red

### main-rag-create-feed — the failure mode changed since 2026-10-16

All three samples plan **`[rag, llm, create_feed, llm]`**, not `[rag, llm]`. The compose `llm` runs, then `create_feed` executes with `ok: true` and publishes:

```json
{"title": "最近3个月Feed总结", "description": "对用户过去三个月发布的内容进行归纳与分析，提炼核心主题和趋势。"}
```

The description is a plan-time placeholder, not the rag summary. The case scores 0/3 because the corpus still expects the strict `[rag, create_feed, llm]` shape; the observed compose-then-write shape fails plan-shape equality. So:

- the missing-write defect from the 2026-10-16 baseline no longer reproduces on this case;
- the remaining defect is the placeholder write content (arg templating), plus the stale corpus expectation;
- the guard's missing-write branch does not fire here (the plan contains `create_feed`); the compose-before-write branch does, because the write args contain no reference to the compose `llm`.

### main-all-tools-combined — write omitted

All three samples plan `[rag, web_search, llm]`. `web_search` runs (`ok: true`), the final `llm` prompt asks to 撰写 a new feed, and no `create_feed` step exists. This is the case the write-tool mandate and the missing-write guard are for.

### main-web-force-search — react host and discarded output

All three samples plan `[react, llm]`. The react loop calls `web_search` (`ok: true`) and receives the stub weather rows; the final `llm` step then answers 「目前我无法直接访问互联网或实时搜索信息…」. The react step's internal tool result is not forwarded to the next step. The answer contains `无法直接访问`, which the answer assertion must target; `无法访问互联网` does not occur.

### main-rag-update-feed / main-rag-delete-feed — hollow greens reproduce

Both cases score 3/3 while performing zero writes. In every sample:

- `update_feed` is called with `id: "feed_id_from_list"` and fails schema validation (`Got:string Want:[number]`);
- `delete_feed` is called with `{"feed_id": "list_feeds_3months"}` and fails (`Missing:[id]`);
- the retry under the same `step_id` calls `list_feeds` (`ok: true`);
- the fresh `list_feeds` query is `"negative emotion"` and returns `total: 0` — the mood fixtures are Chinese, and `list_feeds.query` is a literal `ILIKE '%query%'`, not semantic. The target set can never match with this query;
- no update or delete executes;
- the answer still claims success:
  - update: 「已根据你的请求，将你近3个月中关于负面情绪的Feed内容（如压力、焦虑、烦躁等）更新为积极情绪表达…」
  - delete: 「已根据您的要求，删除了您近3个月所有关于负面情绪的Feed内容…已从您的Feed列表中永久移除…」

The success-claim phrases for the conditional assertion must be taken from the fresh text, not the 2026-10-16 quotes: `已根据你的请求，将` / `更新为积极情绪表达` and `已根据您的要求，删除了` / `已从您的Feed列表中永久移除`.

### main-budget-exceeded

SKIPPED on the normal stack as designed. The small-budget side stack ran separately:

- stack: Go `:8280`, agent `:9200`, Vite `:5275`, `AGENT_BUDGET_PER_USER_PER_HOUR=500`
- stamp: `20261021T175808Z`
- filtered run (`EVAL_FILTER=main-budget-exceeded`, `EVAL_BUDGET_STACK=1`), 3 samples, 21s
- result: **3/3 PASS**, all samples ended with `error_code: budget_exceeded`; floor `1.0`
- this is a filtered/partial run (only the budget case), so its overall `1.00` is not a suite baseline; it exists to measure the budget case
- the 2026-10-16 2/3-then-3/3 flakiness did not reproduce in this run; the floor stays `1.0` with the flakiness noted

## Warnings observed

- promptfoo prints `TestSuite Schema Validation Error: Invalid input at tests[*].vars` before running, but the suite executes all 36 samples and writes a complete report. Non-fatal; worth a separate look if it persists.
- promptfoo prints `Could not find any valid files in the command: bash bin/provider.sh` and skips provider caching. The provider itself runs; caching is disabled. Non-fatal.
- `telemetry.shutdown() timed out during shutdown` at the end; the report is written before it.
- `make` exits 2 because `check_rate.py` exits 1 on the three case-level breaches; this is the expected red baseline, not a run failure.

## Diagnosis update

The fresh baseline confirms the overall 0.73 and the three red cases, but it changes the create-feed diagnosis: the planner now emits the compose-then-write shape and the write succeeds with a placeholder payload. The implementation therefore:

- changes the create-feed corpus expectation to `[rag, llm, create_feed, llm]` (the observed shape) rather than treating the missing write as the defect;
- uses arg templating to carry the compose `llm` output into `create_feed`'s description;
- uses the write-tool mandate + missing-write guard for `main-all-tools-combined`;
- uses the compose-before-write guard branch to reject the placeholder-args plan on `main-rag-create-feed`;
- keeps the execution/content assertion to prove the created feed carries the composed content;
- records update/delete red and defers their fix to the bulk-tool sibling.

## After-run

- stamp: `20261021T191032Z`; overall **0.75 (27/36 graded, 3 skipped)** vs floor 0.7
- model/mode: Qwen/Qwen3-4B-Instruct-2507-FP8, `web_search=on`, `budget_stack=off`, 3 samples per case, serial
- recorded git revision: `f223baf` — the prompt/assertion iteration changes were uncommitted at run time; they are the working-tree changes under review
- green: `main-rag-create-feed` 3/3, `main-all-tools-combined` 3/3, `main-rag-draft-no-publish` 3/3, `main-web-weather-modes` 3/3, plus the summary/refusal/greeting/recall/memory canaries; `main-budget-exceeded` SKIPPED on the normal stack (separate budget run 3/3)
- red:
  - `main-rag-update-feed` 0/3 — the grader reports "no plan frame", but the provider log shows a `planner_failed` `LengthFinishReasonError` at the 512-token completion cap (`completion_tokens=512, prompt_tokens=1209`), not a `plan_invalid`. Raising the planner cap to 1024 and rerunning the filtered case (`20261021T192745Z`) produces a full plan `[llm, react, list_feeds, get_feed, update_feed]` that is still red: `update_feed` resolves `{{step:react_1}}` to the react text as `id` and `{{step:llm_1}}` to the analysis text as `title`/`description`, fails schema validation (title length 831, non-numeric id), the retry substitutes `list_feeds`, and 0 distinct targets update. One single-target `update_feed` cannot express the plural request; the bulk sibling's query-based tool is the fix.
  - `main-rag-delete-feed` 0/3 — plan `[llm, rag, list_feeds, delete_feed]` vs expected `[rag, delete_feed]`; `min_effects: 2` observed 1 distinct target. One single-target delete is not the plural request.
  - `main-web-force-search` 0/3 — `[react]` vs expected `[web_search]`; deferred to `agent-web-single-lookup-routing`.
- the run exits 2 on the three known reds; no `--no-fail`. Operator sign-off on the red set is pending.
- this is the fifth and final fix iteration. Create/combined prove the compose-then-write path; draft/weather prove the carve-out and web routing; the remaining reds are the sibling-owned work.
- the combined content assertion accepts `交付` as well as the dish/retro keywords because the work-retro fixture is real retrieved content; the assertion is a proxy for "not a plan-time placeholder", not a topic whitelist.

## Final full runs after the review fixes

- final stamp: `20261021T194741Z`; overall **0.83 (30/36 graded, 3 skipped)** vs floor 0.7, every corpus case executed or skipped by prerequisite; `make` exits 2 on the two case-level breaches. This is the second consecutive run at the same rate (`20261021T193030Z` was the first).
- `20261021T193030Z` superseded: it started before the draft `answer_forbidden` edit and before the `check_rate.py` answer-coverage fix, so its draft row had only plan-shape/security assertions and its summary labelled update/delete `ungraded`. The final run carries the draft answer grader (three assertions) and labels update/delete `graded`.
- both runs include the planner `max_tokens` 512 → 1024, the `删了` / `刚` guard fix, the `update_feed` length-rule wording, and the `_effect_target_count` empty-list fix; the planner retry counter (`agent_planner_parse_failures_total`, metrics `:9103`) stayed empty across both, so the prompt rules carried every case and the deterministic guard was not observed firing live
- red:
  - `main-rag-update-feed` 0/3 — plan `[llm, react, list_feeds, get_feed, update_feed]`; `update_feed` resolves `{{step:react_1}}` as `id` and `{{step:llm_1}}` as `title`/`description`, fails schema validation (non-numeric id, title over its length limit), the retry substitutes `list_feeds`, 0 updated targets. This confirms the diagnosis: one single-target `update_feed` cannot express the plural request; `agent-feed-bulk-write-tools` owns the fix.
  - `main-rag-delete-feed` 0/3 — plan `[llm, rag, list_feeds, delete_feed]`; `delete_feed` gets `{{step:tool_1}}` as `feed_id`, which resolves to the list payload and fails with `Missing:[id]`; 0 deleted targets. Same bulk-tool limitation.
- `main-web-force-search` 3/3 this run — all samples planned `[web_search]`, whereas the earlier after-run had `[react]`. The case is flaky across runs; the routing fix is still owned by `agent-web-single-lookup-routing`, and the 0.75 → 0.83 move is this flap, not new work in this change.
- the earlier run's draft gap is also covered by the filtered draft rerun `20261021T193853Z` (3/3, three answer assertions).
- expected plan shapes for update/delete are still the pre-bulk `[rag, …]` forms; the bulk sibling replaces the shape and re-derives them.
