# Spec Delta

## MODIFIED Requirements

### Requirement: Per-step SSE frames preserve the existing chat wire contract

The orchestrator SHALL emit SSE frames in this order over a run: one `plan` frame, then for each step one `step_start`, zero or more `tool_call` / `tool_result` / `delta` / `note` / `confirm_request` frames, then one `step_end`, then one `done`, then one `terminator`. The existing `delta`, `done`, `note`, `terminator` shapes MUST match the Go publisher. The `error` frame MAY carry new `code` values; old codes remain valid. A `confirm_request` frame MUST carry the run id, step id, tool name, the tool arguments (masked consistently with `tool_call` frames), an approval id, and an expiry timestamp; within the same step it MUST be followed by either the approved call's regular `tool_call` / `tool_result` frames or a `tool_result` frame carrying the denial.

#### Scenario: New client renders intermediate steps

- **WHEN** a client subscribes and parses every event kind
- **THEN** it sees `plan` first, then per-step frames, then `done`, then `terminator`, and the user sees tool names + final reply in order

#### Scenario: Confirmation request appears inside the paused step

- **WHEN** a step issues a mutating registry call
- **THEN** the client sees `step_start`, then `confirm_request`, and only after an approval decision the corresponding `tool_call` / `tool_result` pair, then `step_end`

#### Scenario: Clients that ignore the new kind are unaffected

- **WHEN** a client parses only the pre-change frame kinds
- **THEN** every pre-change frame keeps its position and shape; the unknown `confirm_request` frame is skippable without misrendering the run

## ADDED Requirements

### Requirement: Approval endpoint accepts decisions under the verified-user-header contract

The service SHALL expose `POST /agent/approvals` accepting `{"run_id": ..., "approval_id": ..., "decision": "approve" | "deny"}`. The endpoint SHALL require the `X-Nyx-User-Id` header exactly like `/agent/run` (same trust boundary, no JWT verification) and MUST reject a missing or non-integer header with HTTP 400 before any state change. The endpoint SHALL verify via the run metadata layer that the run belongs to the header user, and MUST reject non-owned runs with the same response as an unknown run (single sentinel, no existence leak). The first decision recorded for an approval id wins; duplicates SHALL be accepted idempotently (HTTP 200) without changing the recorded outcome. A decision for an unknown or already-expired approval id MUST be rejected with a typed error response (HTTP 404) and MUST never create an approval. The response MUST NOT carry internal error text.

#### Scenario: Missing or non-integer user header rejected

- **WHEN** `POST /agent/approvals` arrives without `X-Nyx-User-Id`, or with a non-integer value
- **THEN** the service returns HTTP 400 with a `detail` naming the header, and no decision is recorded

#### Scenario: Non-owned run is indistinguishable from unknown

- **WHEN** the header user id does not own the run named in the request
- **THEN** the response equals the response for an unknown run and no decision is recorded

#### Scenario: First decision wins, duplicates are idempotent

- **WHEN** an `approve` is recorded for an approval id and a later `deny` arrives for the same id
- **THEN** the later request returns HTTP 200 and the recorded outcome remains `approve`

#### Scenario: Unknown or expired approval rejected

- **WHEN** a decision arrives for an approval id that does not exist or whose TTL has expired
- **THEN** the service returns HTTP 404 and records nothing

### Requirement: New env key for approval TTL

The service SHALL read `PYTHON_AGENT_APPROVAL_TTL_SECONDS` (default 300, must be ≥ 1) — the wall-clock window a mutating-call confirmation stays open before it is closed as denied. The service SHALL refuse to start when the value is below 1.

#### Scenario: Default TTL applied when unset

- **WHEN** the service starts with `PYTHON_AGENT_APPROVAL_TTL_SECONDS` unset
- **THEN** a pending confirmation expires 300 seconds after the `confirm_request` frame is emitted

#### Scenario: Invalid TTL refuses startup

- **WHEN** `PYTHON_AGENT_APPROVAL_TTL_SECONDS` is set to 0 or a negative value
- **THEN** the service fails to start with a clear error naming the key
