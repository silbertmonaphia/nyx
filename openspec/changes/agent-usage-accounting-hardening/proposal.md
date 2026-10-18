# Proposal

## Why

The real-token accounting landed in `e4ddc55` leaves two review findings open. A run resumed from a checkpoint written before the upgrade mixes the old flat estimate with new provider-reported usage, so the done frame and `agent.runs.total_tokens` can report `total_tokens != prompt_tokens + completion_tokens`. Separately, the ambient Langfuse callback handler is preserved only by an accidental-looking `merge_configs` call; no test pins it, so a future simplification would silently stop tracing while the suite stays green.

## What Changes

- Treat a state without `cumulative_prompt_tokens` as a pre-upgrade checkpoint: the first real usage accrual on resume resets `cumulative_tokens` to that delta instead of adding it to the legacy estimate, so the done frame and run row report `total = prompt + completion`.
- Add a regression test that invokes the planner's structured path through a real graph node with an ambient sentinel callback handler and asserts the handler still fires alongside `UsageMetadataCallbackHandler`.
- No wire change: the `done` frame keeps its `{usage: {prompt_tokens, completion_tokens, total_tokens}}` shape; only the values become self-consistent on resume.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `agent-observability`: add the requirement that run-level token accounting is provider-derived and internally consistent, including runs resumed from pre-upgrade checkpoints.

## Impact

- Code: `backend/agent_py/agent/state.py`, `backend/agent_py/agent/planner.py` (test-only touch), `backend/agent_py/tests/test_usage_accounting.py`, `backend/agent_py/tests/test_planner.py`.
- No API, schema, dependency, or configuration changes.
- No rollout gates: the fix and its tests are completable in the dev/CI environment.
