package tool

import "context"

// Tool is the metadata + dispatcher pair the registry stores. The
// registry is the only place a Tool literal is constructed — callers
// (mcp / agent) consume the registry's snapshot and dispatcher.
//
// Name is the lookup key (e.g. "list_feeds"). Version is advisory;
// it surfaces on the wire so a future API can negotiate capability
// sets. ArgType is the zero value of the argument struct (used by
// the registry to build a reflect.Type at registration time) — the
// caller's JSON unmarshal produces a populated value that the
// dispatcher receives as `args any`. Dispatch is the actual work
// function; it MUST read the owning user id from the context (the
// registry refuses to invoke it when the id is missing), run its
// business logic, and return either a typed result or an error.
//
// The dispatcher signature is intentionally `args any` rather than
// `args *T` to keep the registry untyped — callers can register
// tools with any arg struct without the registry having to know the
// type at compile time. The reflect.Type built from ArgType at
// Register time drives struct-tag validation; the type assertion
// happens inside the body where the concrete type is known.
type Tool struct {
	Name        string
	Version     string
	Description string
	ArgType     any
	Dispatch    func(ctx context.Context, args any) (any, error)
}
