// Package tool is the internal tool registry shared by both the MCP
// server (internal/mcp) and the Python agent orchestrator (which
// dispatches via the MCP tools/call JSON-RPC endpoint — see
// openspec/changes/agent-langgraph/design.md §2). It owns:
//   - tool registration (the only place tool metadata lives);
//   - per-owner scoping (reqctx.UserIDFromContext) — dispatcher fails
//     closed when the user id is missing or empty;
//   - input validation via a minimal struct-tag validator (no extra
//     dependencies) so invalid arguments never reach a tool body;
//   - safe-error mapping via api.ClassifyAndLog so internal error text
//     (SQL fragments, bcrypt, JWT parser output, panic traces) never
//     reaches the caller.
//
// The registry does NOT own HTTP, JSON-RPC, or SSE. Those stay in the
// callers (mcp, agent); the registry is a pure Go dispatcher.
package tool

import "errors"

// Sentinel errors returned by Registry.Dispatch. Callers (mcp / agent)
// use errors.Is to translate these into their wire shape. The
// underlying message is intentionally generic so a caller that
// string-compares (which it shouldn't — see CLAUDE.md) still cannot
// leak internals.
var (
	// ErrMissingOwner is returned when reqctx.UserIDFromContext
	// yields 0. The tool body MUST NOT execute. This is the
	// fail-closed path for an unauthenticated dispatch.
	ErrMissingOwner = errors.New("tool: caller has no owning user")

	// ErrInvalidArgs is returned when args fail struct-tag
	// validation against the tool's declared ArgType. The tool body
	// MUST NOT execute. The wrapped FieldError (errors.As) carries
	// the failing field name so the caller can render a useful
	// response.
	ErrInvalidArgs = errors.New("tool: invalid arguments")

	// ErrToolNotFound is returned by Dispatch when the requested
	// name is not registered. The tool body MUST NOT execute.
	ErrToolNotFound = errors.New("tool: not found")

	// ErrPanic is returned when the tool body panicked. The panic
	// value is logged with the request id at warn level via
	// api.ClassifyAndLog; the caller sees only ErrPanic.
	ErrPanic = errors.New("tool: internal panic during dispatch")

	// ErrInternal is returned when the tool body returns an error
	// that is not a recognised domain sentinel and not ErrInvalidArgs
	// / ErrMissingOwner / ErrToolNotFound / ErrPanic. The wrapped
	// error is logged; the caller sees only ErrInternal.
	ErrInternal = errors.New("tool: internal error")
)

// FieldError carries the failing field name returned by the validator.
// Callers use errors.As to extract it and render a structured
// "details" payload.
type FieldError struct {
	Field string
	Err   error
}

func (e *FieldError) Error() string {
	if e.Field == "" {
		return e.Err.Error()
	}
	return e.Field + ": " + e.Err.Error()
}

// Unwrap lets errors.Is traverse to the inner validation error.
func (e *FieldError) Unwrap() error { return e.Err }

// fieldErrorf is a small helper to wrap a per-field error without
// importing fmt at every call site.
func fieldErrorf(field string, err error) error {
	return &FieldError{Field: field, Err: err}
}
