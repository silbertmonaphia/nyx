# agent-user-prefs Specification

## Purpose
Remembers stable user preferences the user states in conversation and feeds them into the planner's system prompt, so the agent stops treating every session like a stranger. Extraction is asynchronous and failure-isolated; injection is additive and preserves the no-prefs prompt byte-for-byte.

## Requirements

### Requirement: Preference persistence with per-key upsert

The service SHALL persist extracted preferences in `agent.chat_user_prefs(id bigserial, user_id bigint, key text, value jsonb, source_session_id uuid NULL, created_at timestamptz, updated_at timestamptz)` with a UNIQUE constraint on `(user_id, key)`. Writing a preference for an existing `(user_id, key)` SHALL update `value`, `source_session_id`, and `updated_at` in place (upsert) — one row per key per user, always reflecting the latest statement.

#### Scenario: First statement inserts

- **WHEN** extraction yields `{key: "snack", value: "鱿鱼丝"}` for user 7 in session S
- **THEN** `agent.chat_user_prefs` gains exactly one row `(7, 'snack', "鱿鱼丝", S, ...)`

#### Scenario: Later statement updates in place

- **WHEN** the same user later states a different snack and extraction yields `{key: "snack", value: "猪肉脯"}` in session T
- **THEN** the existing `(7, 'snack')` row's `value` becomes `"猪肉脯"`, `source_session_id` becomes T, `updated_at` advances
- **AND** no second `(7, 'snack')` row exists

### Requirement: Extraction runs asynchronously after the reply

Preference extraction SHALL run as a fire-and-forget asyncio task started after the assistant message has been recorded for a turn. It MUST NOT delay, block, or alter the SSE stream: the run's `done` and `terminator` frames complete independently of extraction. On any extraction failure (LLM error, malformed output, DB error) the task SHALL log at WARN and end; the failure MUST NOT surface to the user and MUST NOT be retried within the run.

#### Scenario: Slow extraction never delays the stream

- **WHEN** extraction takes 30 seconds while the chat reply stream completes in 2 seconds
- **THEN** the client receives `done` + `terminator` at ~2 seconds; no frame carries extraction state; the prefs row appears later once the task finishes

#### Scenario: Extraction failure is invisible

- **WHEN** the extraction LLM call fails
- **THEN** the chat completes normally, a WARN is logged, no prefs row is written, and the next chat is unaffected

#### Scenario: Extraction never consumes the user chat budget

- **WHEN** a user is at their hourly chat budget cap
- **THEN** chat model calls are refused with `budget_exceeded` while extraction (an internal operation, one small call per preference-bearing turn) proceeds independent of the per-user budget counter

### Requirement: Lightweight gate skips non-preference messages

Extraction SHALL first apply a cheap heuristic (`looks_like_preference`) to the user's message; when the message carries no first-person preference signal the extraction LLM call SHALL NOT fire. The heuristic SHALL cover at least English ("I like/love/prefer/am into") and Chinese ("我喜欢/我爱/偏好/是…迷") first-person preference markers. The heuristic is deliberately conservative-simple; false negatives (a missed preference) are acceptable, false positives (spending an LLM call on small talk) are not.

#### Scenario: Preference-bearing message extracts

- **WHEN** the user writes "我喜欢鱿鱼丝" (or "I really like squid jerky")
- **THEN** the gate passes and the extraction LLM call fires

#### Scenario: Small talk is skipped

- **WHEN** the user writes "今天天气怎么样" (or "thanks!")
- **THEN** the gate fails and no extraction LLM call is made

### Requirement: Planner prompt injection with empty-set parity

Before each plan, the planner SHALL load the user's preferences (`updated_at DESC`, at most `PYTHON_AGENT_PREFS_MAX_INJECTED` rows) and, when non-empty, append a rendered preferences block to the planner system prompt. When the user has no preferences, or `PYTHON_AGENT_PREFS_ENABLED=false`, the planner system prompt SHALL be byte-identical to its pre-injection form.

#### Scenario: Preferences reach the planner

- **WHEN** the user has `{snack: 鱿鱼丝}` and sends a message
- **THEN** the planner system prompt contains the rendered preferences block with that entry; the executor path is unchanged

#### Scenario: Empty prefs leave the prompt untouched

- **WHEN** the user has zero preferences
- **THEN** the planner system prompt bytes are identical to the pre-injection form (regression-test pinned)

#### Scenario: Kill switch disables extraction and injection together

- **WHEN** `PYTHON_AGENT_PREFS_ENABLED=false`
- **THEN** no extraction task fires and the planner prompt omits the block, even for users with stored preferences (rows remain in the table)

### Requirement: Lifecycle follows chat-history retention

Preferences whose `source_session_id` session is hard-deleted by the chat-retention cron SHALL be deleted in the same retention iteration. This keeps derived memory from outliving the conversation it came from. A per-iteration orphan sweep SHALL collect any prefs whose source session no longer exists, so a cleanup failure or a crash between the session delete and the prefs delete cannot strand rows permanently; a counter makes the sweep observable.

#### Scenario: Session hard-delete collects its prefs

- **WHEN** a session is hard-deleted by retention and a prefs row's `source_session_id` is that session
- **THEN** the prefs row is deleted in the same iteration; increments of `agent_chat_retention_deleted_total` are unaffected (prefs are not counted as sessions)

#### Scenario: Stranded prefs are recovered by the orphan sweep

- **WHEN** a prefs cleanup failed (or the process died) after its session rows were hard-deleted, leaving rows whose `source_session_id` matches no `chat_sessions` row
- **THEN** a later retention tick's orphan sweep deletes them and increments `agent_chat_prefs_deleted_total`

#### Scenario: Prefs with other sessions survive

- **WHEN** the same user has prefs sourced from a live session
- **THEN** those rows are untouched by another session's deletion
