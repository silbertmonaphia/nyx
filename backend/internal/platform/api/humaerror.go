package api

import (
	"net/http"

	"nyx/internal/reqctx"

	"github.com/danielgtaylor/huma/v2"
)

// OverrideHumaErrors replaces huma's package-level error constructors so
// every error produced internally by huma (validation failures, panic
// recovery, the WriteErr helpers, the pre-built 4xx/5xx helpers) flows
// through the legacy {error, code, request_id, details} envelope that
// the Vite frontend and the e2e suite already parse.
//
// This works because huma's response pipeline serializes any value that
// satisfies huma.StatusError via the API's JSON marshaller — there is
// no hard dependency on *huma.ErrorModel. By returning *ErrorResponse
// here we make huma marshal our struct (using our JSON tags) instead of
// its RFC 9457 Problem Details shape.
//
// Huma's default validation status is 422 ("Unprocessable Entity"). To
// preserve the gin-era wire contract that the frontend and tests
// already expect (400 for bad request bodies, including schema
// validation), we remap 422 → 400 here. The error envelope, including
// the per-field "details" array, is otherwise unchanged.
//
// Call once at process startup before any huma.Register calls. Tests
// that build their own huma.API should call this in TestMain.
func OverrideHumaErrors() {
	huma.NewError = func(status int, msg string, errs ...error) huma.StatusError {
		return newErrorResponse(mapValidationStatus(status), msg, collectDetails(errs), "")
	}
	huma.NewErrorWithContext = func(ctx huma.Context, status int, msg string, errs ...error) huma.StatusError {
		return newErrorResponse(mapValidationStatus(status), msg, collectDetails(errs), reqctx.RequestIDFromContext(ctx.Context()))
	}
}

// mapValidationStatus rewrites huma's default validation status (422)
// to 400 so the wire contract matches the gin-era behaviour the
// frontend was built against.
func mapValidationStatus(status int) int {
	if status == http.StatusUnprocessableEntity {
		return http.StatusBadRequest
	}
	return status
}

// newErrorResponse builds an ErrorResponse with the given status code,
// user-facing message, optional structured detail, and request ID.
// Pass an empty requestID when no context is available.
func newErrorResponse(status int, msg string, details interface{}, requestID string) *ErrorResponse {
	return &ErrorResponse{
		Message:   msg,
		Code:      status,
		RequestID: requestID,
		Details:   details,
	}
}

// collectDetails folds the variadic error list huma passes into a shape
// suitable for the "details" field. huma's default NewError built a
// []*huma.ErrorDetail slice; we expose the same data under "details"
// as a JSON array of {message, location?, value?} objects so the
// frontend sees the per-field validation messages it expected.
//
// A nil entry is skipped. Any non-ErrorDetailer error is wrapped as
// {message: err.Error()}.
func collectDetails(errs []error) interface{} {
	if len(errs) == 0 {
		return nil
	}
	out := make([]errorDetail, 0, len(errs))
	for _, e := range errs {
		if e == nil {
			continue
		}
		out = append(out, errorDetailFromError(e))
	}
	return out
}

// errorDetailFromError mirrors huma.ErrorDetail but is exported under
// our JSON tags so the wire shape is independent of huma's struct.
func errorDetailFromError(e error) errorDetail {
	if d, ok := e.(huma.ErrorDetailer); ok {
		ed := d.ErrorDetail()
		return errorDetail{Message: ed.Message, Location: ed.Location, Value: ed.Value}
	}
	return errorDetail{Message: e.Error()}
}

// errorDetail is the per-field entry under "details" for multi-error
// responses (validation, etc.). Field names mirror huma.ErrorDetail so
// existing frontend consumers continue to work.
type errorDetail struct {
	Message  string `json:"message"`
	Location string `json:"location,omitempty"`
	Value    any    `json:"value,omitempty"`
}
