# agent-run-store-py Delta

## MODIFIED Requirements

### Requirement: `RunMetadata.create_run` mints a `run_id` and persists a metadata row

The metadata layer SHALL mint a `run_id` (uuid4) on `create_run(user_id, session_id=None) -> uuid`, INSERT a row into `agent.runs`, and return the new uuid. The row SHALL carry `status='running'`, `started_at=now()`, `last_heartbeat_at=now()`, `user_id` from the upstream header, `total_tokens=0`, `error_code=NULL`, `ended_at=NULL`, and `session_id` set to the given conversation-thread id or NULL when the caller has none. The row does NOT carry `plan_json` — that lives in langGraph's checkpoint blob. The same lifecycle rules apply to every row regardless of what kind of turn minted it: continuation turns and fresh sessions produce rows that are indistinguishable in schema and lifecycle (heartbeat, single terminal transition, sweeper reaping, retention GC).

#### Scenario: Fresh run inserts a metadata row with the right shape

- **WHEN** the agent mints a `run_id` and calls `RunMetadata.create_run(user_id)`
- **THEN** `agent.runs` contains one row with `id = <uuid>`, `user_id = <user_id>`, `status = 'running'`, `started_at` within the last second, `last_heartbeat_at` within the last second, `ended_at IS NULL`, `total_tokens = 0`, `session_id IS NULL`

#### Scenario: Continuation turn links its row to the session

- **WHEN** a session-continuation turn calls `RunMetadata.create_run(user_id, session_id="<thread-uuid>")`
- **THEN** `agent.runs` contains one row with `session_id = <thread-uuid>` and the same running-state shape as a fresh-run row; the echoed session id itself keeps whatever rows it historically had, untouched
