# Tasks

## 1. Executor: forward react output

- [ ] 1.1 Add a `react` branch to `_render_prior_data` (`backend/agent_py/agent/executor.py:575-628`) rendering a completed react step's text as a labelled context section, reusing the existing untrusted-marker stripping and both character caps rather than introducing a second bounding policy.
- [ ] 1.2 Add tests that a `[react, llm]` plan's answer step receives the react text, that a react step reporting failure is forwarded as failure text, and that the caps still bound a multi-section context with an explicit truncation marker.
- [ ] 1.3 Verify the `delta` frames the client receives from a react step, and their order, are byte-identical before and after — the forwarding is a prompt-context change only. Verify with a recorded frame fixture.

## 2. Planner rule for single lookups

- [ ] 2.1 Extend `_PLANNER_RULE` (`backend/agent_py/agent/web_tools.py:520-524`) to state that a request for a single lookup plans one `web_search` `tool` step, and that `react` is for work needing multi-round interaction. Verify the composed planner prompt renders.
- [ ] 2.2 Verify the rule does not conflict with `agent-web-tools`' requirement that web tools stay dispatchable from both `tool` and `react` steps — it narrows when `react` is chosen, it does not remove the capability. Verify with the existing web-tools tests under the agent venv.

## 3. Corpus and answer assertion

- [ ] 3.1 Add a forbidden-keyword assertion to `main-web-force-search` in `evals/cases/main.yaml` using strings that actually occur in the recorded failure (「无法获取」, 「无法直接访问」). Verify `evals/validate.py` passes.
- [ ] 3.2 Verify the assertion **fails on the recorded baseline sample** and passes on a search-grounded answer. An assertion whose keyword never occurs in the recorded failure is vacuous coverage, not coverage.
- [ ] 3.3 Add a corpus case that legitimately needs multi-round interaction, so the single-lookup preference is not only tested in the direction that lowers the plan's step count. Verify a `react` expectation remains reachable and passes.
- [ ] 3.4 Add grader unit tests under `evals/tests` for the new answer assertions, parsed against the recorded frame fixture from the force-search sample.

## 4. Docs

- [ ] 4.1 Document the single-lookup preference and the react output forwarding in `backend/agent_py/README.md` (and `README.zh.md`), including that `react` remains supported. Verify the documented behaviour matches the shipped rule and executor branch.