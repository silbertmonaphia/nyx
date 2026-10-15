# Spec Delta

## ADDED Requirements

### Requirement: Over-long streams fail closed at a configurable cap

The proxy SHALL bound each upstream streaming call with a configurable wall-clock cap on the upstream request context — never a fixed client-level timeout that also bounds the streamed body, which severs healthy streams with no terminal frames. When the cap expires while the upstream stream is still live, the handler SHALL close the wire contract with a synthesized `error{code:"stream_timeout"}` frame followed by `done` and `terminator`, and the upstream request MUST be cancelled. The synthesized frames are a deliberate, bounded exception to byte-for-byte forwarding — sibling to the unreachable-agent branch — because the agent never got to emit its own terminator. When the upstream has already forwarded its own terminal frames (`done`, `terminator`, or the `[DONE]` sentinel) before the cap fires, the handler MUST NOT append any further frame: the stream is not double-terminated.

#### Scenario: Cap expires mid-stream

- **WHEN** the upstream stream is still live as the configured cap expires with the client attached
- **THEN** the client receives `error{code:"stream_timeout"}` + `done` + `terminator` on that connection and the upstream request is cancelled

#### Scenario: A healthy stream shorter than the cap is untouched

- **WHEN** the upstream stream completes normally before the cap
- **THEN** the body is forwarded byte-for-byte and no synthesized frame is appended

#### Scenario: No double-termination after the upstream's own terminal frames

- **WHEN** the cap expires after the upstream already forwarded its own terminal frames (its body held open past them)
- **THEN** nothing is appended and the client sees exactly the upstream bytes

#### Scenario: Client disconnect takes precedence

- **WHEN** the client disconnects before the cap expires
- **THEN** the upstream call is cancelled by the request context and no synthesized frame is written
