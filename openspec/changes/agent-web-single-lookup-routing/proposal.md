# Proposal

## Why

`main-web-force-search` is 0/3 and has been 0/6 across two runs. On 「今天天气如何？**上网搜索看看**」 the planner emits `[react, llm]` where the case expects `[web_search, llm]`. There is no code switch that chooses between them — `react` and `tool` are both first-class step kinds, and the web-tools planner rule advertises `web_search` without ever steering away from a loop. The 4B reads the imperative 「上网搜索看看」 as "run a search loop" and the bare question 「今天天气如何？」 as "one lookup", which is why the sibling weather case reaches the flat shape and this one does not.

Worse, the react shape does not even deliver the result. The loop **did** call `web_search` and **did** receive real weather rows, and then the final `llm` step answered 「我目前无法获取今天全国主要城市的详细天气状况…」. The cause is architectural, not model error: `_render_prior_data` (`backend/agent_py/agent/executor.py:575-628`) forwards only `rag` and `tool` step outputs into a later step's context, so a `react` step's text is dropped. The model is asked to answer from search results it was never given.

The case grades plan shape only, so the discarded answer shipped silently.

## What Changes

- **Planner rule for single lookups.** `_PLANNER_RULE` (`backend/agent_py/agent/web_tools.py:520-524`) gains an explicit preference: a request for one lookup plans one `web_search` `tool` step; `react` is reserved for work that genuinely needs multi-round interaction. This narrows *when* `react` is chosen and does not remove the capability — `agent-web-tools` requires web tools to remain dispatchable from both `tool` and `react` steps.
- **React step outputs reach the next step.** `_render_prior_data` forwards a completed `react` step's text into later steps' context alongside `rag` and `tool` outputs, within the existing per-section and total character caps. Without this the flat-shape fix alone would still let an answer be written from a context that never received the search.
- **An answer assertion on the force-search case** that fires on the denial phrasing actually present in the recorded failure (「无法获取」 / 「无法直接访问」), verified against the recorded sample — not a paraphrase, which would be a vacuous assertion that never fires.

**Note on capability placement:** the plan-shape requirement for single-lookup routing is carried in `agent-planner-write-routing`'s `agent-plan-shape-policy` delta, because that capability does not exist until that change is archived. This change carries the executor-side data-flow requirement and the implementation of both.

## Capabilities

### New Capabilities

- None.

### Modified Capabilities

- `agent-loop`: a completed `react` step's output is carried into later steps' context the way `rag` and `tool` outputs already are, bounded by the existing caps.

## Impact

- **Code**: `backend/agent_py/agent/web_tools.py` (`_PLANNER_RULE`), `backend/agent_py/agent/executor.py` (`_render_prior_data` react branch), `evals/cases/main.yaml` (answer assertion), `evals/graders/` (assertion wiring), `evals/tests/` (tests against the recorded frame fixture).
- **Not touched**: no Go. `web_search` / `web_fetch` are agent-native and are not exposed through the MCP server; the only Go mention of `web_search` is a comment in `internal/llm/provider.go` distinguishing it from a vLLM provider-specific kind.
- **Behaviour**: user-visible. A request that names web search returns search-derived answers instead of an apology, and a single lookup stops being wrapped in a multi-round loop.
- **Ordering**: no implementation dependency on the other slice-2 change (`agent-feed-bulk-write-tools`); both are sequenced together for rate reasons only. The plan-shape requirement it implements lives in `agent-planner-write-routing`, so that change should archive first.
- **Rollout gates**: none. No production env flip, no deployed-stack smoke, no multi-day green window.