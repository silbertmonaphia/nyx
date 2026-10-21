# Spec Delta

## MODIFIED Requirements

### Requirement: Answer-content grading

A case MAY carry both plan-shape and answer-content assertions, and the two SHALL be graded independently — a sample fails if either fails. A case carrying only plan-shape assertions leaves answer quality ungraded by construction, and the report MUST make that coverage gap visible so a plan-shape-only case is never read as evidence that the answer was correct. Answer-content assertions SHALL be deterministic content checks on the answer text assembled from all of the run's `delta` frames in wire order (`done` carries usage only and no text), including deltas emitted inside a `react` step and deltas emitted by the final `llm` step: required keywords/phrases MUST be present, forbidden keywords MUST be absent. Matching is a case-sensitive substring check, so a forbidden keyword MUST be chosen from the phrasing that actually appears in the observed failure text; a paraphrase that the transcript never contains does not exercise the assertion and MUST NOT be treated as coverage. Forbidden-keyword assertions are the mechanism for catching a run that performed the work but denied it in the reply — for example, a transcript whose `web_search` `tool_call` returned results while the answer claims the agent cannot search. The harness MUST NOT use an LLM judge in v0.

#### Scenario: Memory recall answer graded by keyword

- **WHEN** the memory scenario's recall turn answers with text containing 秋刀鱼
- **THEN** the keyword assertion passes regardless of surrounding phrasing

#### Scenario: Plan-shape and answer assertions grade independently

- **WHEN** a case declares both a plan-shape expectation and an answer-content assertion
- **THEN** the sample passes only if the observed plan tokens match the expectation AND the answer text satisfies the assertion; either failure fails the sample and the report names which assertion failed

#### Scenario: Plan-shape-only case reports its answer coverage as ungraded

- **WHEN** a case declares only a plan-shape expectation
- **THEN** the report marks that case's answer coverage as ungraded, so a green rate on the case is not read as evidence about the answer text

#### Scenario: Performed-but-denied answer fails the forbidden-keyword assertion

- **WHEN** a run's transcript contains a successful `web_search` `tool_call` and `tool_result`, and the answer text asserts the agent has no internet access
- **THEN** the forbidden-keyword assertion fails the sample, and the report records the successful tool result alongside the contradicting answer text

#### Scenario: Forbidden keyword is verified against the recorded failure

- **WHEN** a forbidden-keyword assertion is added to a case whose recorded failing answer says 「无法直接访问互联网」
- **THEN** the assertion uses a phrase that occurs in that text (for example 「无法直接访问」), and a paraphrase such as 「无法访问互联网」 that does not occur is rejected as vacuous coverage

### Requirement: Rates over verdicts, floors as exit codes

The harness SHALL run each case N times (repeat count configurable, default 3, serial — one model instance) and report a pass rate per case and overall. A case passes when its rate meets its corpus `min_rate` floor; the run exits non-zero when any case breaches its floor or the overall rate is below the configured overall floor, and exits zero under an explicit no-fail/trend flag regardless of floors. Every sample's transcript SHALL be available for triage. Floors are set from a baseline run, and a baseline below the sanity threshold (under 1/3 of samples) MUST NOT be written as a floor: such a case is diagnosed (expectation, approval policy, or product bug) and either fixed or explicitly recorded red with operator sign-off. The report SHALL record the overall floor actually applied by the run together with the baseline rate it was derived from, so a report can never be read against a floor the runner did not enforce. A run that did not execute every case — cases reported NOT RUN, or SKIPPED beyond the case's declared prerequisites — MUST NOT be recorded as a baseline, and its overall rate MUST NOT be presented as evidence that the suite passed.

#### Scenario: Red baseline is not silenced

- **WHEN** a case passes 0 of 3 baseline samples
- **THEN** the harness workflow requires diagnosis and operator sign-off before a `min_rate` floor is written for it, rather than recording a floor that the red rate already satisfies

#### Scenario: One-in-three case with a 1.0 floor fails the run

- **WHEN** a case with `min_rate: 1.0` passes 1 of 3 samples and the overall floor is not breached
- **THEN** the run exits non-zero and the report shows the case rate 0.33 with the failing sample's tokens

#### Scenario: Trend run reports without failing

- **WHEN** the run is invoked with the no-fail flag and floors are breached
- **THEN** the report is produced and the exit code is zero

#### Scenario: Report names the floor and its baseline provenance

- **WHEN** a run is graded with an overall floor that differs from the harness's built-in default
- **THEN** the report records the applied floor value and the baseline rate it was derived from, not the default, so a later reader cannot mistake which gate the run cleared

#### Scenario: Partial run is not a baseline

- **WHEN** a run executes only one case (for example, a budget-stack run that did not run the main line) and every other case is NOT RUN
- **THEN** the report marks the other cases NOT RUN and the overall rate is not recorded or cited as a baseline for the suite

### Requirement: Live-gated execution posture

The suite SHALL run only against a live model stack (the local vLLM and the running agent service), never as part of the default CI test gates. Samples SHALL run serially (one model instance), and live runs SHALL target a side stack brought up through the repo's established side-stack convention rather than the operator's own dev processes. The harness's own deterministic logic (corpus validation, graders, transcript parsing) SHALL be covered by unit tests that do not require the model, parsing recorded frame fixtures from a real run rather than synthetic ones; the unit gate runs under the agent venv (`pytest evals/tests`) and runs in CI as part of the Python agent's `agent-lint` job, with the job's path filter covering `evals/**`, so changes to graders, corpus validation, or transcript parsing fail a pull request rather than surfacing at the next manual eval run.

#### Scenario: Default CI never runs the live suite

- **WHEN** the project's default test commands run (agent-free)
- **THEN** no eval-suite case calls the model; only the deterministic unit tests execute

#### Scenario: Grader tested against recorded wire frames

- **WHEN** the grader unit tests run
- **THEN** they parse recorded SSE fixture files produced by a real run, not hand-built in-process objects

#### Scenario: CI runs the deterministic eval unit gate

- **WHEN** the Python agent's CI job runs
- **THEN** `pytest evals/tests` executes as part of that job, with no model stack required, and a failing grader or corpus-validation test fails the job

#### Scenario: Eval-only change triggers the unit gate

- **WHEN** a pull request changes only `evals/**`
- **THEN** the `agent-lint` job still runs and executes `pytest evals/tests`

## ADDED Requirements

### Requirement: Execution-effect grading for state-changing cases

A case that expects a mutating registry tool (for example `create_feed`, `update_feed`, `delete_feed`, or the bulk `update_feeds` / `delete_feeds` added by `agent-feed-bulk-write-tools`) SHALL grade the run's execution, not only its plan. The transcript SHALL contain a `tool_call` for the expected write tool under the expected step, and the corresponding `tool_result` SHALL report `ok: true`. A sample MUST fail when the plan names the write tool but the executed call fails validation, when a retry substitutes a different tool, or when the tool result is missing. Where the case asserts a specific feed, the harness SHALL verify the effect from the `tool_result` payload when it carries the created/updated feed (the `create_feed` result does), falling back to the eval user's API when it does not (the feed exists / was changed / was removed); for a create case whose contract is composed content, it SHALL also verify that the created feed's content reflects the retrieved data rather than a plan-time placeholder. A write-expecting case that declares no execution assertion MUST fail corpus validation rather than report a plan-shape-only green.

#### Scenario: Failed write call fails the sample despite a matching plan

- **WHEN** the plan contains `update_feed` but its `tool_result` reports `ok: false` (for example, a schema-validation error), and the retry calls `list_feeds`
- **THEN** the sample fails execution-effect grading even though the plan tokens match the expectation

#### Scenario: Retry substitution fails the sample

- **WHEN** the plan contains `create_feed`, but the only executed `tool_call` under that step is a different tool
- **THEN** the sample fails execution-effect grading and the report records the executed tool name

#### Scenario: Successful write with the expected effect passes

- **WHEN** the plan contains `create_feed`, the `tool_result` reports `ok: true`, and the eval user's feed list contains the newly created feed with the expected composed content
- **THEN** the sample passes both plan-shape and execution-effect grading

#### Scenario: Created feed carries the composed content

- **WHEN** a create case expects a summary of retrieved feeds and the created feed's description contains a plan-time placeholder instead of a keyword from the retrieved fixtures
- **THEN** the sample fails execution-effect grading even though the feed row exists

#### Scenario: Write case without execution assertions is a corpus bug

- **WHEN** a case declares a write-tool plan expectation but no execution-effect assertion
- **THEN** corpus validation fails loudly instead of allowing a plan-shape-only green
