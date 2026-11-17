# Spec Delta

## MODIFIED Requirements

### Requirement: Resumed runs emit `note{kind:"resumed"}` before the plan

When `POST /agent/run` arrives with a `run_id` whose `agent.runs` row exists, is owned by the request's `user_id`, and is non-terminal (`status NOT IN ('completed', 'failed')`), the service SHALL build `config = {"configurable": {"thread_id": str(run_id)}}` and continue the run from the latest checkpoint for that thread. Each plan step SHALL execute in its own graph superstep, and a checkpoint SHALL be committed at every step boundary before the next step begins, so a plan with N steps yields N step checkpoints after the planner checkpoint. On resume the service SHALL continue at the first step with no checkpointed terminal outcome; a step whose outcome is already checkpointed MUST NOT be re-executed. Only the step that was in flight when the previous process stopped MAY run again. Frames already emitted for completed steps MAY be replayed from the checkpoint under the existing replay semantics; such a replay MUST NOT cause the step to be re-executed. The SSE stream SHALL emit `event: note\ndata: {"kind": "resumed", "last_step_id": "<id>"}\n\n` immediately before the `plan` frame. When `RunMetadata.get_run(run_id, user_id)` returns `None` (not found, wrong user, or terminal), the service SHALL mint a fresh `run_id` and emit `event: note\ndata: {"kind": "fresh_run", "run_id": "<uuid>"}\n\n` instead, then proceed with the new plan.

#### Scenario: Resume rehydrates from langGraph's last checkpoint

- **WHEN** `POST /agent/run` arrives with a `run_id` whose `agent.runs` row is `status='running'` and langGraph's checkpoint at `thread_id=<run_id>` records that k plan steps have completed and step k+1 was in flight
- **THEN** the SSE stream emits `note{kind:"resumed"}` with `last_step_id` naming the last checkpointed step, then the `plan` frame, and execution continues at step k+1; steps 1..k are not re-executed (token usage and tool-dispatch counts confirm no double-spend)

#### Scenario: A crash during a step re-runs only that step

- **WHEN** the agent process is killed while step k+1 is executing, after steps 1..k completed and their per-step checkpoints were committed
- **THEN** a resumed run re-executes only step k+1; every earlier step's output is restored from its checkpoint and no earlier step's handler, model call, or tool dispatch is invoked again

#### Scenario: A crash after the final step replays only the terminal frames

- **WHEN** the process stops after the final plan step's checkpoint was committed but before the run's `done` / `terminator` frames were emitted
- **THEN** the resumed run executes no step again and emits the remaining terminal frames from the checkpointed state

#### Scenario: Cross-user resume attempt is treated as fresh

- **WHEN** `POST /agent/run` arrives with a `run_id` whose `agent.runs` row exists but with a different `user_id` from the request's `X-Nyx-User-Id`
- **THEN** `RunMetadata.get_run(run_id, other_user_id)` returns `None` and the service mints a fresh `run_id`, emitting `note{kind:"fresh_run"}` followed by the new plan; the existing run is left untouched and no checkpoint data is read

#### Scenario: Terminal metadata row attempt is treated as fresh

- **WHEN** `POST /agent/run` arrives with a `run_id` whose `agent.runs` row is `status='completed'` or `'failed'`
- **THEN** `RunMetadata.get_run` returns `None` and the service mints a fresh `run_id`, emitting `note{kind:"fresh_run"}` followed by the new plan; no checkpoint data is read
