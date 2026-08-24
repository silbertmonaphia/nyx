package api

import (
	"context"
	"errors"
	"net/http"
)

// mapping is the canonical status + message pair for a single domain
// sentinel. Kept tiny — just enough for the handler layer to render a
// response without knowing HTTP. New domain errors register here once
// at process startup; the registry is keyed by the sentinel itself so
// errors.Is walks the chain and lands on the right mapping.
type mapping struct {
	status  int
	message string
}

// mappings is the sentinel → wire-shape lookup. Built lazily via
// RegisterSentinel so the api package has no upstream dependency on
// any domain package — domains register their own sentinels at
// startup (mirroring how main.go wires the rest of the system).
var mappings = map[error]mapping{}

// RegisterSentinel binds a domain sentinel to its HTTP status + wire
// message. Last writer wins on duplicate registrations; the function
// is intended to be called once per sentinel from an init() or a
// setup hook in main.go.
func RegisterSentinel(sentinel error, status int, message string) {
	mappings[sentinel] = mapping{status: status, message: message}
}

// MapError returns the canonical *ErrorResponse for err, falling back
// to a 500 envelope when err matches no registered sentinel. The
// unknown-error path reuses ClassifyAndLog so the underlying error is
// logged at Warn with the request ID but never reaches the response
// `details` field — handlers stay one line long on the error path.
//
// safeDetail is the per-call-site string surfaced to the client. It
// MUST be a static literal; never pass err.Error() here. The helper
// itself does not know which operation it serves, by design: callers
// stay in control of the public-facing wording.
//
// Returns nil for a nil err so handlers can write
// `return nil, api.MapError(ctx, err, "...")` unconditionally.
func MapError(ctx context.Context, err error, safeDetail string) *ErrorResponse {
	if err == nil {
		return nil
	}
	for sentinel, m := range mappings {
		if errors.Is(err, sentinel) {
			return NewErrorResponseFromContext(ctx, m.status, m.message, nil)
		}
	}
	return NewErrorResponseFromContext(ctx, http.StatusInternalServerError, "Internal server error", ClassifyAndLog(ctx, err, safeDetail))
}