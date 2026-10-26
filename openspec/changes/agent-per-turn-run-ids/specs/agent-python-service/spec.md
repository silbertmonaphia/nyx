# agent-python-service Delta

## MODIFIED Requirements

### Requirement: Session continuation on echoed session ids

When `POST /agent/run` arrives with a `run_id` matching a live (non-deleted) `chat_sessions` row owned by the requesting user, the service SHALL treat the request as a continuation of that session, and the execution SHALL receive its own freshly minted per-turn `run_id` with a full `agent.runs` row (status `running`, heartbeat maintained, exactly one terminal transition): the SSE `run_id` frame SHALL carry the new per-turn id, and the client-echoed id SHALL remain the session (thread) identity for chat-history writes. The per-turn `agent.runs` row SHALL record the owning session id. A non-terminal `agent.runs` row for the echoed id takes priority — crash-recovery resume semantics are unchanged. Unknown ids, another user's ids, and soft-deleted sessions SHALL fall through to a freshly minted session. Only the LAST user message of the request is appended on a continuation (the SPA re-sends the session's full history every turn; earlier messages already exist). There is no idle-time session chopping: the client's `+ New chat` is the only session-boundary signal. LangGraph checkpoint threading SHALL key on the per-turn run id; a continuation turn starts a fresh plan, so no checkpoint rehydration occurs for it.

#### Scenario: Continuation appends without duplication

- **WHEN** turn 2 of a session arrives echoing the session id with history `[u1, a1, u2]`
- **THEN** the SSE `run_id` frame carries a fresh per-turn id different from the echoed id; the session (addressed by the echoed id) gains exactly one `u2` row and one new assistant row; `u1`/`a1` are not re-recorded; exactly one new `agent.runs` row exists for this turn and it records the session id

#### Scenario: API-created session receives its first turn

- **WHEN** a session was created via `POST /agent/sessions` (no prior `agent.runs` row) and its first chat echoes its id
- **THEN** the turn is appended to it and the execution mints a per-turn `agent.runs` row linked to that session

#### Scenario: Per-turn row follows the normal lifecycle

- **WHEN** a continuation turn completes
- **THEN** its `agent.runs` row transitions to a terminal state exactly once (with cumulative token total), the turn's heartbeat is maintained while it executes, and a turn that stalls past the heartbeat deadline is reaped by the stale-run sweeper like any other running row

#### Scenario: Old sessions keep their thread

- **WHEN** a session whose last activity is 30 days old is echoed
- **THEN** the turn is appended to it — there is no idle-time chopping

#### Scenario: Unknown and cross-user ids mint fresh

- **WHEN** the echoed id is unknown, owned by another user, or soft-deleted
- **THEN** the service mints a fresh session id for the turn; the other session (if any) is left untouched

### Requirement: Approval endpoint accepts decisions under the verified-user-header contract

The service SHALL expose `POST /agent/approvals` accepting `{"run_id": ..., "approval_id": ..., "decision": "approve" | "deny"}`. The endpoint SHALL require the `X-Nyx-User-Id` header exactly like `/agent/run` (same trust boundary, no JWT verification) and MUST reject a missing or non-integer header with HTTP 400 before any state change. The endpoint SHALL verify that the header user owns the run named in the request via the run metadata layer; for `run_id` values minted before per-turn run rows existed (legacy conversations with no `agent.runs` row), the endpoint SHALL additionally accept ownership proven by a live, non-deleted `chat_sessions` row with that id owned by the header user. MUST reject non-owned runs with the same response as an unknown run (single sentinel, no existence leak), including when the ownership stores are unavailable (fail closed). The first decision recorded for an approval id wins; duplicates SHALL be accepted idempotently (HTTP 200) without changing the recorded outcome. A decision for an unknown or already-expired approval id MUST be rejected with a typed error response (HTTP 404) and MUST never create an approval. The response MUST NOT carry internal error text.

#### Scenario: Missing or non-integer user header rejected

- **WHEN** `POST /agent/approvals` arrives without `X-Nyx-User-Id`, or with a non-integer value
- **THEN** the service returns HTTP 400 with a `detail` naming the header, and no decision is recorded

#### Scenario: Non-owned run is indistinguishable from unknown

- **WHEN** the header user id does not own the run named in the request
- **THEN** the response equals the response for an unknown run and no decision is recorded

#### Scenario: Per-turn run row proves ownership

- **WHEN** a `confirm_request` was raised inside a continuation turn and the decision names that turn's per-turn run id, with the header user owning the `agent.runs` row
- **THEN** the decision is recorded without consulting the session table

#### Scenario: Legacy session-owned run still decidable

- **WHEN** a decision names an id that has no `agent.runs` row (a conversation started before per-turn run ids) and a live, non-deleted `chat_sessions` row with that id is owned by the header user
- **THEN** the decision is recorded; when the session row is missing, foreign, soft-deleted, or the store errors, the response is the single 404 sentinel and nothing is recorded

#### Scenario: First decision wins, duplicates are idempotent

- **WHEN** an `approve` is recorded for an approval id and a later `deny` arrives for the same id
- **THEN** the later request returns HTTP 200 and the recorded outcome remains `approve`

#### Scenario: Unknown or expired approval rejected

- **WHEN** a decision arrives for an approval id that does not exist or whose TTL has expired
- **THEN** the service returns HTTP 404 and records nothing
