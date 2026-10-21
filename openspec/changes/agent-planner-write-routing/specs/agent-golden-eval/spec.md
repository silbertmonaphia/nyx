# Spec Delta

## MODIFIED Requirements

### Requirement: Answer-content grading

A case MAY carry both plan-shape and answer-content assertions, and the two SHALL be graded independently — a sample fails if either fails. A case carrying only plan-shape assertions leaves answer quality ungraded by construction, and the report MUST make that coverage gap visible so a plan-shape-only case is never read as evidence that the answer was correct. Answer-content assertions SHALL be deterministic content checks on the answer text assembled from the run's `delta` frames (`done` carries usage only and no text): required keywords/phrases MUST be present, forbidden keywords MUST be absent. Forbidden-keyword assertions are the mechanism for catching a run that performed the work but denied it in the reply — for example, a transcript whose `web_search` `tool_call` returned results while the answer claims the agent cannot search. The harness MUST NOT use an LLM judge in v0.

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

### Requirement: Rates over verdicts, floors as exit codes

The harness SHALL run each case N times (repeat count configurable, default 3, serial — one model instance) and report a pass rate per case and overall. A case passes when its rate meets its corpus `min_rate` floor; the run exits non-zero when any case breaches its floor or the overall rate is below the configured overall floor, and exits zero under an explicit no-fail/trend flag regardless of floors. Every sample's transcript SHALL be available for triage. Floors are set from a baseline run, and a baseline below the sanity threshold (under 1/3 of samples) MUST NOT be written as a floor: such a case is diagnosed (expectation, approval policy, or product bug) and either fixed or explicitly recorded red with operator sign-off. The report SHALL record the overall floor actually applied by the run together with the baseline rate it was derived from, so a report can never be read against a floor the runner did not enforce.

#### Scenario: Red baseline is not silenced

- **WHEN** a case passes 0 of 3 baseline samples
- **THEN** the harness workflow requires diagnosis and operator sign-off before a `min_rate` floor is written for it, rather than recording a floor that the red rate already satisfies

#### Scenario: One-in-three case with a 1.0 floor fails the run

- **WHEN** a case with `min_rate: 1.0` passes 1 of 3 samples and the overall floor is not breached
- **THEN** the run exits non-zero and the report shows the case rate 0.33 with the failing sample's tokens

#### Scenario: Trend run reports without failing

- **WHEN** the run is invoked with the no-fail flag and floors are breached
- **THEN** the report is produced and the exit code is zero

#### Scenario: Report names the floor it was graded against

- **WHEN** a run is graded with an overall floor that differs from the harness's built-in default
- **THEN** the report records the applied floor value and the baseline rate it was derived from, not the default, so a later reader cannot mistake which gate the run cleared

### Requirement: Live-gated execution posture

The suite SHALL run only against a live model stack (the local vLLM and the running agent service), never as part of the default CI test gates. Samples SHALL run serially (one model instance), and live runs SHALL target a side stack brought up through the repo's established side-stack convention rather than the operator's own dev processes. The harness's own deterministic logic (corpus validation, graders, transcript parsing) SHALL be covered by unit tests that do not require the model, parsing recorded frame fixtures from a real run rather than synthetic ones; the unit gate runs under the agent venv (`pytest evals/tests`) and runs in CI as part of the Python agent's lint-and-test job, so changes to graders, corpus validation, or transcript parsing fail a pull request rather than surfacing at the next manual eval run.

#### Scenario: Default CI never runs the live suite

- **WHEN** the project's default test commands run (agent-free)
- **THEN** no eval-suite case calls the model; only the deterministic unit tests execute

#### Scenario: Grader tested against recorded wire frames

- **WHEN** the grader unit tests run
- **THEN** they parse recorded SSE fixture files produced by a real run, not hand-built in-process objects

#### Scenario: CI runs the deterministic eval unit gate

- **WHEN** the Python agent's CI job runs
- **THEN** `pytest evals/tests` executes as part of that job, with no model stack required, and a failing grader or corpus-validation test fails the job