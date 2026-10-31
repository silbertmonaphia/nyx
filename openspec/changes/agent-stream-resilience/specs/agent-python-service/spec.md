# Spec Delta

## Purpose

Makes the run stream survivable: a keepalive that intermediaries respect, a run whose lifetime outlives its connection, and an endpoint a reconnecting client can use to observe the same run instead of re-executing it.

## MODIFIED Requirements

### Requirement: Terminal persistence survives client disconnect

The SSE generator's terminal persistence (the assistant message row, the session `ended_at` stamp, and the `agent.runs` terminal transition for non-continuation runs) SHALL complete even when the client disconnects at or after the `terminator` frame. The writes SHALL run in a task shielded from generator cancellation so a fully-streamed answer can never lose its stored assistant half or leave its run row stuck `running`.

Run execution SHALL be decoupled from the response stream by default: with `PYTHON_AGENT_STREAM_DETACH_ENABLED=true` (the default), a client disconnect before `done` SHALL NOT cancel the run — the run continues to a terminal state in a supervisor task that keeps the run's heartbeat advancing, is bounded by `PYTHON_AGENT_DETACHED_RUN_CAP_SECONDS`, and performs the same terminal persistence as an attached run. An echoed `run_id` whose run is detached-and-live SHALL resolve to the live-run (crash-recovery resume / re-attach) branch of the existing resume decision tree, exactly as any other non-terminal run. When the cap expires before the run reaches a terminal state, the run SHALL be marked `failed` with a typed error code and SHALL emit its normal terminal frames to any attached subscriber. Setting `PYTHON_AGENT_STREAM_DETACH_ENABLED=false` restores the legacy semantics: a client disconnect before `done` cancels the run generator exactly as before this change.

#### Scenario: Disconnect at terminator still persists the answer

- **WHEN** a client closes the stream immediately after the `terminator` frame (the SPA parser's normal close point) while the terminal writes are mid-flight
- **THEN** the assistant row lands in `chat_messages`, the session's `ended_at` is stamped, and the run row reaches `completed` — none of them depend on the client staying connected

#### Scenario: Mid-run disconnect keeps the legacy resumable semantics when detach is opted out

- **WHEN** a client disconnects before the run reaches `done` with `PYTHON_AGENT_STREAM_DETACH_ENABLED=false`
- **THEN** no assistant row is written, the run row stays `running` (resumable inside the stale-run window; the sweeper reaps it otherwise), and the run generator is cancelled — the pre-change semantics, available as the kill switch

#### Scenario: Detached run completes after the client leaves

- **WHEN** a client disconnects mid-run with `PYTHON_AGENT_STREAM_DETACH_ENABLED=true`
- **THEN** the run continues executing in the background, its `agent.runs` heartbeat keeps advancing until terminal, and on completion the assistant row, the session `ended_at` stamp, and the terminal run transition land exactly as for an attached run

#### Scenario: Detached run is bounded

- **WHEN** a detached run exceeds `PYTHON_AGENT_DETACHED_RUN_CAP_SECONDS` without reaching a terminal state
- **THEN** the run is marked `failed` with a typed error code and no process-level task leaks

#### Scenario: Detached run answers the resume decision tree as a live run

- **WHEN** a client re-POSTs `/agent/run` echoing the id of a detached, still-live run
- **THEN** the request is handled as a live-run resume (`resumed=True` with the `note{kind:"resumed"}` frame), not as a session continuation or a fresh run

## ADDED Requirements

### Requirement: Run-stream keepalive cadence is operator-configurable

The service already emits an SSE comment frame (`: keep-alive` followed by a blank line, no `event` field, no `data` field) after stream idleness on the run stream; the re-attach stream SHALL carry the same behavior. The idle interval SHALL be configurable via `PYTHON_AGENT_KEEPALIVE_SECONDS` (integer ≥ 0, default 15, replacing the hardcoded constant). A configured value of `0` SHALL disable the keepalive. Comment frames SHALL NOT alter the framing of adjacent event frames and SHALL NOT be counted as stream activity that defers the terminal frames.

#### Scenario: Idle stream keeps receiving keep-alives

- **WHEN** a long-running tool call produces no event frames for longer than the configured interval
- **THEN** the stream carries `: keep-alive\n\n` frames at the configured cadence between the surrounding event frames

#### Scenario: Keepalive disabled by config

- **WHEN** `PYTHON_AGENT_KEEPALIVE_SECONDS=0`
- **THEN** no comment frames are emitted and the stream is byte-identical to a keepalive-free run

#### Scenario: Default keeps today's cadence

- **WHEN** `PYTHON_AGENT_KEEPALIVE_SECONDS` is unset
- **THEN** the keepalive behaves exactly as the pre-change hardcoded 15 s behavior

#### Scenario: Old consumers are unaffected

- **WHEN** a parser that ignores comment lines (the SPA's, per the SSE spec) receives a stream containing keep-alives
- **THEN** every `event`/`data` frame it yields and the terminal `[DONE]` sentinel are identical to a keepalive-free stream

### Requirement: Re-attach endpoint exposes a live run's stream

The service SHALL expose `GET /agent/runs/{run_id}/stream` under the service-token middleware with the `X-Nyx-User-Id` header required exactly like `/agent/run`. For a run that is live (non-terminal in `agent.runs`) and owned by the header user, the endpoint SHALL return HTTP 200 with the standard SSE headers and emit, in order: a `note{kind:"reattached"}` frame, the run's persisted step events replayed as their normal `plan` / `step_start` / `tool_call` / `tool_result` / `step_end` frames, a re-emitted `confirm_request` frame if the run is paused awaiting an approval decision, then the run's live frames until the terminal trio. The replayed stream SHALL carry the keep-alive comments. A run that is not live, unknown, or owned by another user SHALL receive one indistinguishable typed error response (HTTP 409, `code:"run_not_live"`) with no existence leak. When the persistence layer is not configured, the endpoint SHALL respond HTTP 501 with `code:"reattach_unavailable"`. The endpoint SHALL NOT mutate the run row, the checkpoint, or the step trace — a re-attach is a view, not a resume.

#### Scenario: Live run re-attaches with replay then live frames

- **WHEN** the endpoint is called for a live owned run that has completed two tool steps and is streaming its answer
- **THEN** the client receives the reattach note, the two steps' frames replayed, then the answer's live `delta` frames, then `done` + `[DONE]`

#### Scenario: Pending approval is re-emitted on re-attach

- **WHEN** the run is paused awaiting an approval decision when the re-attach arrives
- **THEN** the stream carries the step replay followed by a `confirm_request` frame with the same `approval_id` and remaining TTL, and the run stays paused until the decision lands

#### Scenario: Non-live, unknown, and cross-user runs are indistinguishable

- **WHEN** the endpoint is called respectively for a terminal run, a nonexistent run id, and another user's live run
- **THEN** all three receive the same HTTP 409 `code:"run_not_live"` envelope, and no response distinguishes which case fired

#### Scenario: Persistence disabled answers 501

- **WHEN** the endpoint is called while the service runs without the run-metadata layer
- **THEN** the client receives HTTP 501 with `code:"reattach_unavailable"` and no stream is opened

### Requirement: Cancel endpoint deliberately terminates a detached run

Because a detached run no longer dies with its connection, the service SHALL expose `POST /agent/runs/{run_id}/cancel` (service-token + `X-Nyx-User-Id` contract, same as `/agent/run`) as the deliberate stop channel. For a run supervised in the detached registry and owned by the header user, the endpoint SHALL cancel the run's supervisor: the pump stops, the run row is marked `failed` with the typed error code `user_cancelled`, no assistant row is written for the partial answer, and any attached or re-attached stream receives an `error{code:"user_cancelled"}` frame followed by the terminal trio. A run that is not supervised (terminal, unknown, another user's, or detach opted out so no registry entry exists) SHALL receive one indistinguishable typed error response (HTTP 409, `code:"run_not_live"`) with no existence leak. The cancelled run's terminal state SHALL make the next echoed `run_id` resolve as a session continuation — a cancelled run is never resurrected by a resume.

#### Scenario: Cancel stops a detached run

- **WHEN** the endpoint is called for a live detached run
- **THEN** the pump is cancelled, the run row lands `failed` with code `user_cancelled`, no assistant row is written, and the subscriber's stream closes with the typed error frame and the terminal trio

#### Scenario: Cancel is idempotent at the identity level

- **WHEN** the endpoint is called for a run that is not supervised or not owned by the caller
- **THEN** the response is the same 409 `run_not_live` envelope as the re-attach endpoint's, and nothing about the run's existence or ownership leaks

#### Scenario: A cancelled run does not resurrect

- **WHEN** the client sends the next message echoing a cancelled run's id
- **THEN** the resume decision tree sees a terminal row and treats the id as a session continuation — a new turn on the same thread, not a resume

### Requirement: New env keys for stream resilience

The service SHALL accept `PYTHON_AGENT_KEEPALIVE_SECONDS` (integer ≥ 0, default 15), `PYTHON_AGENT_STREAM_DETACH_ENABLED` (boolean, default `true`), and `PYTHON_AGENT_DETACHED_RUN_CAP_SECONDS` (integer ≥ 1, default 600). A negative keepalive interval, a negative or zero detached-run cap, or a non-integer value in any of the three SHALL refuse startup with a message naming the key. Defaults SHALL keep an unconfigured deployment booting with today's keepalive cadence and a bounded detach window.

#### Scenario: Defaults applied when unset

- **WHEN** none of the three keys is set
- **THEN** keep-alives run at 15 s, detach is on, the cap is 600 s, and the service starts

#### Scenario: Invalid cap refuses startup

- **WHEN** `PYTHON_AGENT_DETACHED_RUN_CAP_SECONDS` is zero or negative
- **THEN** startup is refused with a message naming `PYTHON_AGENT_DETACHED_RUN_CAP_SECONDS`

#### Scenario: Detach opted out needs no cap

- **WHEN** `PYTHON_AGENT_STREAM_DETACH_ENABLED=false` and the cap key is unset
- **THEN** startup succeeds; the cap is unused

#### Scenario: Keepalive zero is a valid disable

- **WHEN** `PYTHON_AGENT_KEEPALIVE_SECONDS=0`
- **THEN** startup succeeds with the keepalive disabled
