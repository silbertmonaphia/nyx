package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"nyx/internal/reqctx"
)

// TestErrorResponse_ImplementsErrorAndStatusError pins the dual
// interface satisfaction. huma.StatusError is the contract that lets
// huma serialize the envelope directly when a handler returns
// *ErrorResponse; stdlib `error` is what lets the same value flow
// through ordinary error pipelines. A regression that drops
// GetStatus would break huma's status-code path.
func TestErrorResponse_ImplementsErrorAndStatusError(t *testing.T) {
	e := &ErrorResponse{Message: "boom", Code: http.StatusBadRequest}

	var stdErr error = e
	if stdErr.Error() != "boom" {
		t.Errorf("Error() = %q, want %q", stdErr.Error(), "boom")
	}
	if e.GetStatus() != http.StatusBadRequest {
		t.Errorf("GetStatus() = %d, want %d", e.GetStatus(), http.StatusBadRequest)
	}
}

// TestWriteError_RoundTrip exercises the helper used by middleware
// that needs to abort a request before any handler runs. The body
// must be the legacy envelope, content-type must be application/json,
// the request ID stamped on context must propagate.
func TestWriteError_RoundTrip(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req = req.WithContext(reqctx.WithRequestID(req.Context(), "req-123"))

	WriteError(rr, req, http.StatusForbidden, "nope", "extra detail")

	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusForbidden)
	}
	if got := rr.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var env ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\nbody=%s", err, rr.Body.String())
	}
	if env.Message != "nope" {
		t.Errorf("envelope.error = %q, want %q", env.Message, "nope")
	}
	if env.Code != http.StatusForbidden {
		t.Errorf("envelope.code = %d, want %d", env.Code, http.StatusForbidden)
	}
	if env.RequestID != "req-123" {
		t.Errorf("envelope.request_id = %q, want %q", env.RequestID, "req-123")
	}
	if env.Details != "extra detail" {
		t.Errorf("envelope.details = %v, want %q", env.Details, "extra detail")
	}
}

// TestNewErrorResponseFromContext_StampsRequestID verifies the
// helper used by huma middleware (which only has a context.Context).
func TestNewErrorResponseFromContext_StampsRequestID(t *testing.T) {
	ctx := reqctx.WithRequestID(context.Background(), "ctx-req-id")

	e := NewErrorResponseFromContext(ctx, http.StatusBadRequest, "bad", nil)
	if e.RequestID != "ctx-req-id" {
		t.Errorf("RequestID = %q, want %q", e.RequestID, "ctx-req-id")
	}
	if e.Code != http.StatusBadRequest {
		t.Errorf("Code = %d, want %d", e.Code, http.StatusBadRequest)
	}
	if e.Message != "bad" {
		t.Errorf("Message = %q, want %q", e.Message, "bad")
	}
}

// TestNewErrorResponseFromContext_MissingRequestID covers the path
// where no RequestID middleware ran (e.g. during early bootstrap).
// The envelope must omit the request_id field — its `omitempty`
// JSON tag is the wire contract.
func TestNewErrorResponseFromContext_MissingRequestID(t *testing.T) {
	e := NewErrorResponseFromContext(context.Background(), http.StatusInternalServerError, "boom", nil)

	body, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(body, []byte("request_id")) {
		t.Errorf("body should not contain request_id when none was set: %s", body)
	}
}

// TestEncodeError_WritesValidJSON ensures EncodeError (used by huma
// middleware) emits the same envelope as WriteError's json.NewEncoder
// call. A regression that produced a different shape would break the
// round-trip symmetry across the two write paths.
func TestEncodeError_WritesValidJSON(t *testing.T) {
	var buf bytes.Buffer
	e := &ErrorResponse{Message: "encoded", Code: http.StatusConflict, RequestID: "r-1"}

	if err := EncodeError(&buf, e); err != nil {
		t.Fatalf("EncodeError: %v", err)
	}

	var got ErrorResponse
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\nbuf=%s", err, buf.String())
	}
	if got.Message != e.Message || got.Code != e.Code || got.RequestID != e.RequestID {
		t.Errorf("round-trip mismatch: got=%+v, want=%+v", got, *e)
	}
}

// TestWriteError_NoRequestIDOmitsField verifies the same omitempty
// contract on the WriteError path. Two write paths (WriteError vs
// EncodeError) must produce identical JSON when the context carries
// no request ID.
func TestWriteError_NoRequestIDOmitsField(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	WriteError(rr, req, http.StatusInternalServerError, "boom", nil)

	body := rr.Body.String()
	if bytes.Contains([]byte(body), []byte("request_id")) {
		t.Errorf("body should not contain request_id when none was set: %s", body)
	}
}

// captureLogger swaps the global zerolog logger out for one that writes
// into the returned buffer, and registers a t.Cleanup to restore the
// previous logger. Tests must call this serially because zerolog.Logger
// is a process-wide singleton; the helper also guards against accidental
// concurrent capture with a panic.
func captureLogger(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := log.Logger
	log.Logger = zerolog.New(buf).Level(zerolog.WarnLevel)
	t.Cleanup(func() { log.Logger = prev })
	return buf
}

// TestClassifyAndLog_NilErrorReturnsSafeDetail pins the nil-error
// passthrough: when the caller has nothing to log, the supplied safe
// detail must be returned verbatim with no log output produced.
//
// captureLogger swaps the global zerolog.Logger; tests that use it
// must NOT call t.Parallel(). Adding parallelism here would race the
// captured buffer against sibling tests' log writes.
//
//nolint:paralleltest
func TestClassifyAndLog_NilErrorReturnsSafeDetail(t *testing.T) {
	buf := captureLogger(t)

	got := ClassifyAndLog(context.Background(), nil, "safe message")
	if got != "safe message" {
		t.Errorf("returned %v, want %q", got, "safe message")
	}
	if buf.Len() != 0 {
		t.Errorf("expected no log output on nil error, got: %s", buf.String())
	}
}

// TestClassifyAndLog_HidesErrorDetails is the wire-level guarantee of
// commit 4: raw err.Error() must NEVER reach the response payload. The
// caller asks for a static safe string and the helper returns exactly
// that, while logging the raw error with the request ID for operators.
//
// captureLogger swaps the global zerolog.Logger; tests that use it
// must NOT call t.Parallel(). Adding parallelism here would race the
// captured buffer against sibling tests' log writes.
//
//nolint:paralleltest
func TestClassifyAndLog_HidesErrorDetails(t *testing.T) {
	buf := captureLogger(t)

	rawErr := errors.New("pgx: SQLSTATE 42P01 relation does not exist")
	ctx := reqctx.WithRequestID(context.Background(), "req-abc")

	got := ClassifyAndLog(ctx, rawErr, "Operation failed")

	if got != "Operation failed" {
		t.Errorf("returned %v, want %q (raw error must not be echoed)", got, "Operation failed")
	}

	logged := buf.String()
	if !strings.Contains(logged, "pgx: SQLSTATE 42P01 relation does not exist") {
		t.Errorf("expected underlying error logged for operators, got: %s", logged)
	}
	if !strings.Contains(logged, "req-abc") {
		t.Errorf("expected request_id req-abc in log output, got: %s", logged)
	}
}

// TestClassifyAndLog_IncludesRequestID exercises the helper with a
// caller-supplied request ID; the warn-level log line must carry it so
// operators can correlate the suppressed error with the request that
// triggered it.
//
// captureLogger swaps the global zerolog.Logger; tests that use it
// must NOT call t.Parallel(). Adding parallelism here would race the
// captured buffer against sibling tests' log writes.
//
//nolint:paralleltest
func TestClassifyAndLog_IncludesRequestID(t *testing.T) {
	buf := captureLogger(t)

	ctx := reqctx.WithRequestID(context.Background(), "test-req-123")

	got := ClassifyAndLog(ctx, errors.New("internal boom"), "ok")

	if got != "ok" {
		t.Errorf("returned %v, want %q", got, "ok")
	}
	logged := buf.String()
	if !strings.Contains(logged, "test-req-123") {
		t.Errorf("expected request_id test-req-123 in log, got: %s", logged)
	}
	if !strings.Contains(logged, "internal boom") {
		t.Errorf("expected underlying error string in log, got: %s", logged)
	}
}
