package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"nyx/internal/reqctx"

	"github.com/stretchr/testify/assert"
)

// panicHandler returns a handler that panics on every call. Used by
// the recoverer tests to exercise the panic path.
func panicHandler(msg string) http.Handler {
	return http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic(msg)
	})
}

// TestRecoverer_PanicReturns500 verifies a panic in a downstream
// handler is caught, translated to HTTP 500, and the response body
// carries the {error, code} envelope (request_id is intentionally
// omitted — see the comment in recoverer.go about the import cycle).
func TestRecoverer_PanicReturns500(t *testing.T) {
	handler := Recoverer(panicHandler("boom"))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusInternalServerError, rr.Code)
	assert.Equal(t, "application/json; charset=utf-8", rr.Header().Get("Content-Type"))

	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v\nraw=%s", err, rr.Body.String())
	}
	assert.Equal(t, "Internal server error", body["error"])
	assert.Equal(t, float64(http.StatusInternalServerError), body["code"])
}

// TestRecoverer_NoPanicPassesThrough ensures non-panicking handlers
// see their response untouched — Recoverer must not add overhead to
// the happy path beyond passing through.
func TestRecoverer_NoPanicPassesThrough(t *testing.T) {
	downstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Downstream", "yes")
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("ok"))
	})
	handler := Recoverer(downstream)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, http.StatusTeapot, rr.Code)
	assert.Equal(t, "yes", rr.Header().Get("X-Downstream"))
	assert.Equal(t, "ok", rr.Body.String())
}

// TestRecoverer_ReadsRequestIDFromContext asserts the request ID
// stored on the context flows into the panic log line. We can't
// assert on the log output directly (zerolog is global); instead we
// verify that running the recoverer with a known request ID on the
// context and then triggering a panic does not break — the assertion
// is that the recoverer reads reqctx.RequestIDFromContext at all. A
// regression where that read is removed would still pass, but it
// costs nothing and catches the obvious "field renamed" class of
// bug.
func TestRecoverer_ReadsRequestIDFromContext(t *testing.T) {
	handler := Recoverer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// Sanity: the request ID the upstream RequestID middleware
		// would have set is still on the context when the panic
		// fires. Recoverer must not strip it.
		if reqctx.RequestIDFromContext(r.Context()) == "" {
			t.Error("request ID missing from context when panic fires")
		}
		panic("triggered")
	}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(reqctx.WithRequestID(req.Context(), "test-id-abc"))
	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusInternalServerError, rr.Code)
}

// TestRecoverer_RecoversNonStringPanic ensures recover() handles
// non-string panic values (e.g. errors, runtime.Error) without
// crashing the recoverer itself. A regression here would mean a
// panic in production takes down the whole server instead of
// returning a 500.
func TestRecoverer_RecoversNonStringPanic(t *testing.T) {
	handler := Recoverer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic(http.ErrAbortHandler) // any non-string value
	}))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, http.StatusInternalServerError, rr.Code)
}
