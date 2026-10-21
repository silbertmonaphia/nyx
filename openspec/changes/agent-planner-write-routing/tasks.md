# Tasks

## 1. Baseline measurement (before any code change)

- [ ] 1.1 Bring up the eval stack through the repo's established side-stack convention (`scripts/e2e-side-stack.sh` with a stack suffix, on spare ports) and verify the Go API `/api/feeds` and the agent `/healthz` both respond. Do not reuse, restart, or occupy the operator's own dev processes or ports — `make eval-run` preflights and refuses to start services itself.
- [ ] 1.2 Run `make eval-golden` on unmodified HEAD and verify the report under `evals/artifacts/<stamp>/` shows all 12 main cases graded with a non-trivial sample count — explicitly not a 3-sample partial of the kind that produced the false green at `20261016T143912Z`.
- [ ] 1.3 Record the confirmed per-case and overall rates in `baseline.md` in this change directory next to the 2026-10-16 figures, and verify the recorded 0.73 either reproduces or is corrected with the new number stated.

## 2. Planner prompt: write-tool mandate

- [ ] 2.1 Add the write-tool mandate and its draft carve-out to `PLANNER_SYSTEM_PROMPT` (`backend/agent_py/agent/planner.py:76-84`), reconciling the new rule with the existing `when unsure, prefer a single llm step` rule in the same edit rather than appending a rule that contradicts it. Verify the composed prompt renders through the existing planner tests without error.
- [ ] 2.2 Confirm the existing planner test suite still passes, and record in the change directory that compliance is graded **only** by the live eval — no deterministic unit test can assert that a 4B model emits a write-tool step, and a prompt-contains assertion would pass without proving compliance.
- [ ] 2.3 Document the write-tool policy and its draft carve-out in `backend/agent_py/README.md` (and `README.zh.md`), and verify the documented behaviour matches the shipped prompt text.

## 3. Web tools prompt: single-lookup preference

- [ ] 3.1 Extend `_PLANNER_RULE` (`backend/agent_py/agent/web_tools.py:520-524`) to prefer one `web_search` `tool` step for a single lookup and reserve `react` for genuinely multi-round requests. Verify `pytest` under the agent venv still passes for the web-tools module.
- [ ] 3.2 Verify the rule text is consistent with `agent-web-tools`' existing requirement that web tools are dispatchable from **both** `tool` and `react` steps — the change narrows when `react` is chosen, it does not remove the capability.

## 4. Eval corpus: answer assertions and red tracking

- [ ] 4.1 Add answer-content assertions to the three affected cases in `evals/cases/main.yaml`, including a forbidden-keyword assertion on the force-search case targeting the denial phrasing specifically (无法访问互联网 / cannot access the web) rather than broad vocabulary. Verify the corpus still passes `evals/validate.py`.
- [ ] 4.2 Correct the `main-rag-create-feed` note that claims the update/delete siblings pass "on the same fixtures" — they use a different fixture set (mood-\* vs food-\*) — and restore the diagnostic text for all three open reds into the corpus case notes. Do not reinstate `bugs.md`; the corpus is the durable home (design.md Decision 5).
- [ ] 4.3 Re-derive the `main-budget-exceeded` floor from the fresh baseline in task 1.2 and record the operator sign-off in the case note. The evidence is two runs five minutes apart at 2/3 and 3/3 (the budget guard's trip point depends on real token spend); if the fresh run is 3/3 the floor stays at 1.0 with the flakiness noted, if it is lower the floor follows the spec's sanity rule and is not written from the red rate alone.
- [ ] 4.4 Add grader unit tests under `evals/tests` for the new answer assertions, parsed against a recorded frame fixture from a real run, and verify `pytest evals/tests` passes locally under the agent venv.

## 5. Report: floor provenance and answer-coverage marking

- [ ] 5.1 Record the overall floor actually applied by the run and the baseline rate it was derived from in both `report.json` and `summary.md` (`evals/bin/check_rate.py`), and verify a run graded at a non-default floor reports that floor rather than the `DEFAULT_MIN_RATE` constant.
- [ ] 5.2 Mark a case's answer coverage as ungraded in the report when it carries only a plan-shape expectation, and verify a plan-shape-only case's green rate cannot be read as evidence about answer text.
- [ ] 5.3 Add `evals/tests` coverage for both report changes and verify the unit gate passes.

## 6. CI: wire the deterministic eval unit gate

- [ ] 6.1 Add `pytest evals/tests` to the `agent-lint` job in `.github/workflows/ci.yml` and extend that job's path filters to cover `evals/**` — the job currently filters on `backend/agent_py/**` only, so a grader change would not trigger it. Resolve the `working-directory` so the path reaches the repo-root `evals/tests`.
- [ ] 6.2 Verify the gate actually fires: introduce a deliberately failing grader test on a scratch branch and confirm the job goes red, then revert. A wired-but-untriggered gate is the exact failure this task exists to prevent.
- [ ] 6.3 Document the gate's presence and what it does not cover (it never calls the model) in `backend/agent_py/README.md`, and verify the documented scope matches the workflow.

## 7. Scheduled live-run feasibility spike

- [ ] 7.1 Investigate whether an unattended live eval run is viable on this project's CI (GPU runner availability, vLLM warm-up cost, how an infra failure would surface) and write the finding as a recommendation in this change directory. Deliver a written recommendation, not a workflow file.
- [ ] 7.2 Write the manual-run fallback runbook into `backend/agent_py/README.md` covering side-stack bring-up, the serial `--workers=1` requirement, and artifact location, so the suite has a documented operating procedure regardless of the spike's outcome.
- [ ] 7.3 If the spike concludes an unattended run is viable, do not build it here — record that it needs its own change with its own spec delta to `agent-golden-eval`'s live-gated posture, and link that recommendation from this change.

## 8. Post-change verification and evidence

- [ ] 8.1 Re-run `make eval-golden` against the edited prompts and verify the after-report: the three target cases moved, **and** `main-rag-update-feed`, `main-rag-delete-feed`, `main-smalltalk-greeting`, and `main-refusal-direct-answer` did not regress. Check the mutation siblings before celebrating the gains.
- [ ] 8.2 Record the after-run in `baseline.md` with the observed plans for any case that did not move. A re-run that does not improve the rate is a valid recorded outcome, not a prompt to keep tuning until the number moves — design.md Decision 3.
- [ ] 8.3 Commit the new baseline report as evidence under `evals/artifacts/<stamp>/`, force-added past the bulk artifact gitignore, mirroring how the 2026-10-16 baselines were landed.