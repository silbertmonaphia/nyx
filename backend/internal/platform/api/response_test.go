package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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