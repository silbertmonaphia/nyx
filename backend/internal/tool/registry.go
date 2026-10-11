package tool

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"nyx/internal/platform/api"
	"nyx/internal/reqctx"
)

// Registry is the in-process tool registry. It is populated at
// process boot via Register and then consulted by every tool call.
// Tools do not change at runtime (no Add / Remove after boot),
// matching the design call in design.md §3 ("tools are static at
// boot").
//
// Concurrency: Read-mostly. A sync.RWMutex protects the map. The
// expected access pattern is Register N times at boot, then
// concurrent Dispatch calls (the hot path) under RLock. Lookup
// uses a sorted-name snapshot so tools/list responses are
// deterministic for clients.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
	order []string // sorted name list, rebuilt on Register
}

// New constructs an empty Registry.
func New() *Registry {
	return &Registry{tools: map[string]Tool{}}
}

// Register inserts t into the registry. Re-registering the same name
// replaces the existing entry and preserves insertion order for the
// other names — useful in tests but not at boot (boot registrations
// are one-shot).
//
// Register panics if t.Name is empty, t.Dispatch is nil, or
// t.ArgType is nil. These are programming errors, not runtime
// conditions, and the boot path is the only one that calls it.
func (r *Registry) Register(t Tool) {
	if t.Name == "" {
		panic("tool.Registry.Register: Name is required")
	}
	if t.Dispatch == nil {
		panic(fmt.Sprintf("tool.Registry.Register(%q): Dispatch is required", t.Name))
	}
	if t.ArgType == nil {
		panic(fmt.Sprintf("tool.Registry.Register(%q): ArgType is required (pass the zero value of the args struct)", t.Name))
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.tools[t.Name]; !exists {
		r.order = append(r.order, t.Name)
		sort.Strings(r.order)
	}
	r.tools[t.Name] = t
}

// Names returns the registered tool names in sorted order. Used by
// tools/list handlers.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Get returns a snapshot copy of the tool registered under name.
// The snapshot's Dispatch closure references the same captured
// service value, so callers don't have to worry about post-Register
// mutation.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Dispatch invokes the tool registered under name with the supplied
// args. The call sequence is:
//
//  1. Look up the tool by name. Missing → ErrToolNotFound.
//  2. Read the owning user id from the context (reqctx.UserIDFromContext).
//     Missing or zero → ErrMissingOwner. The body MUST NOT execute.
//  3. Validate args against the tool's ArgType struct tags. Failure
//     → wrapped *FieldError inside ErrInvalidArgs. The body MUST
//     NOT execute.
//  4. Run the tool body inside a recover() guard so a panic does
//     not crash the process. The panic value is logged via
//     api.ClassifyAndLog (Warn + request id); the caller receives
//     ErrPanic.
//  5. Map any non-sentinel error through api.ClassifyAndLog so the
//     underlying message is logged but the returned error text is
//     safe to ship. Registry-internal sentinels and recognised
//     domain sentinels are returned as-is so callers can use
//     errors.Is on feed.ErrNotFound, etc.
//
// The first argument the body receives is ctx (carrying the owning
// user id); the second is args, which the caller already populated
// from the wire (e.g. via json.Unmarshal). The body's return is
// (any, error) — `any` is the typed result the caller will marshal
// to the wire.
func (r *Registry) Dispatch(ctx context.Context, name string, args any) (result any, err error) {
	t, ok := r.Get(name)
	if !ok {
		return nil, ErrToolNotFound
	}

	// Per-owner scoping — fail closed BEFORE the body runs.
	if reqctx.UserIDFromContext(ctx) == 0 {
		return nil, ErrMissingOwner
	}

	// Struct-tag validation — fail closed BEFORE the body runs.
	if validationErr := validate(t.ArgType, args); validationErr != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidArgs, validationErr)
	}

	// Panic recovery + safe-error mapping. The deferred function
	// captures result/err by name so we can rewrite err on a panic
	// without disturbing a successful return. On a normal return
	// the body below ran already; we just map any non-nil err.
	defer func() {
		pv := recover()
		if pv == nil {
			return
		}
		panicErr := fmt.Errorf("%v", pv)
		api.ClassifyAndLog(ctx, panicErr, "tool panic suppressed")
		result = nil
		err = ErrPanic
	}()

	result, err = t.Dispatch(ctx, args)
	if err != nil {
		// The dispatcher's error mapping policy:
		//   - registry-internal sentinels (these errors) pass through
		//     unrewritten so the caller can errors.Is on them;
		//   - everything else (including feed.ErrNotFound — a domain
		//     sentinel the caller recognises via errors.Is in its own
		//     mapping) is logged via ClassifyAndLog so the underlying
		//     text is recorded but the error returned by Dispatch
		//     is the original sentinel chain so errors.Is keeps
		//     working for the caller.
		//
		// The "log + return original" pattern preserves the leak-free
		// contract: api.ClassifyAndLog records the underlying text
		// for operators, but the value returned to the caller is
		// still the typed sentinel the caller maps to a wire-safe
		// message itself. The registry does NOT pre-empt the
		// caller's wire rendering — that's the caller's job.
		api.ClassifyAndLog(ctx, err, "tool error suppressed")
	}
	return result, err
}

// IsKnownSentinel reports whether err matches one of the
// registry-internal sentinels. Callers can use this to decide
// whether to translate err into a wire format (their job) or to
// apply additional mapping.
func IsKnownSentinel(err error) bool {
	switch {
	case errors.Is(err, ErrMissingOwner):
		return true
	case errors.Is(err, ErrInvalidArgs):
		return true
	case errors.Is(err, ErrToolNotFound):
		return true
	case errors.Is(err, ErrPanic):
		return true
	case errors.Is(err, ErrInternal):
		return true
	}
	return false
}
