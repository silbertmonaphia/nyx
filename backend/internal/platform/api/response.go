// Package api holds cross-cutting HTTP response shapes. The ErrorResponse
// envelope is the canonical JSON shape for every error returned by the
// Nyx API. Handlers, middleware, and huma operations all funnel their
// errors through WriteError or by returning *ErrorResponse directly so
// the envelope stays identical across the codebase.
//
// ErrorResponse also implements huma.StatusError, so huma operations
// can `return nil, &api.ErrorResponse{...}` and huma will write the
// JSON body using these tags. The legacy envelope is preserved
// end-to-end because OverrideHumaErrors routes huma's own error
// constructors through the same struct.
package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/rs/zerolog/log"

	"nyx/internal/reqctx"
)

// ClassifyAndLog returns safeDetail for inclusion in the API response
// `details` field, and logs the underlying err with the request ID at
// Warn level so operators can correlate. Callers MUST pass a static
// safeDetail — never err.Error() — to avoid leaking SQL, connection
// strings, or library internals to clients.
//
// nil-safe: when err is nil, safeDetail is returned unchanged.
func ClassifyAndLog(ctx context.Context, err error, safeDetail any) any {
	if err == nil {
		return safeDetail
	}
	reqID := reqctx.RequestIDFromContext(ctx)
	log.Warn().
		Str("request_id", reqID).
		Err(err).
		Msg("internal error suppressed from response details")
	return safeDetail
}

// ErrorResponse defines the standard JSON structure for all API errors.
// It also implements huma.StatusError, so handlers can return a
// pointer to it directly and huma will marshal the same JSON shape.
//
// The Go field name is `Message` rather than `Error` because Go does
// not permit a struct field and a method to share a name; the JSON tag
// keeps the on-wire key as `"error"` so the wire contract is unchanged.
type ErrorResponse struct {
	Message   string      `json:"error"`
	Code      int         `json:"code"`
	RequestID string      `json:"request_id,omitempty"`
	Details   interface{} `json:"details,omitempty"`
}

// Error makes ErrorResponse satisfy the standard error interface.
// It returns the user-facing message, which is what huma's default
// error logging uses.
func (e *ErrorResponse) Error() string { return e.Message }

// GetStatus makes ErrorResponse satisfy huma.StatusError so huma
// writes the supplied HTTP status instead of its default 500.
func (e *ErrorResponse) GetStatus() int { return e.Code }

// WriteError serializes an ErrorResponse to w with the given status
// code. The request ID is read from r.Context() so middleware-produced
// errors carry the same correlation ID as the corresponding log line.
//
// Returns immediately after writing; callers should `return` afterwards.
func WriteError(w http.ResponseWriter, r *http.Request, statusCode int, message string, details interface{}) {
	e := newErrorResponse(statusCode, message, details, reqctx.RequestIDFromContext(r.Context()))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(e)
}

// NewErrorResponseFromContext builds an ErrorResponse with the request
// ID stamped from ctx. Used by huma middleware that has only a
// context.Context (no http.Request).
func NewErrorResponseFromContext(ctx context.Context, status int, message string, details interface{}) *ErrorResponse {
	return newErrorResponse(status, message, details, reqctx.RequestIDFromContext(ctx))
}

// EncodeError writes an ErrorResponse as JSON to w. Kept as a
// package-level helper so huma middleware (which has only
// io.Writer-shaped BodyWriter) can reuse the same encoding path
// without reaching for encoding/json directly.
func EncodeError(w io.Writer, e *ErrorResponse) error {
	return json.NewEncoder(w).Encode(e)
}
