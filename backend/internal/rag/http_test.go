package rag

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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	"nyx/internal/reqctx"
)

// captureLogger swaps log.Logger for a buffer-backed logger so
// tests can assert on log entries without polluting test output.
// Restores the original logger on test teardown.
func captureLogger(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	orig := log.Logger
	log.Logger = zerolog.New(buf).Level(zerolog.WarnLevel)
	t.Cleanup(func() { log.Logger = orig })
	return buf
}

// httpCtxWithUser stamps a user id on r's context, mirroring what
// middleware.NewAuth does in production. The bridge handler reads
// the value via reqctx.UserIDFromContext.
func httpCtxWithUser(r *http.Request, userID int) *http.Request {
	return r.WithContext(reqctx.WithUserID(r.Context(), userID))
}

// userCapturingRetriever is a minimal RetrieverIface stub for
// the HTTP handler tests. It records the userID it was called
// with so the JWT-wins test can assert the bridge handed it the
// verified JWT's id, not the query param's id. It can also be
// seeded with canned passages or an error.
type userCapturingRetriever struct {
	passages []Passage
	err      error
	seenID   *int
}

func (r *userCapturingRetriever) Retrieve(_ context.Context, userID int, _ []float32, _ int) ([]Passage, error) {
	if r.seenID != nil {
		*r.seenID = userID
	}
	if r.err != nil {
		return nil, r.err
	}
	return r.passages, nil
}

// newStubService builds a real *Service wired to a stub embedder +
// the capturing retriever. Tests pass this to Handler(svc) so the
// HTTP path can be exercised against canned responses without
// touching pgvector.
func newStubService(t *testing.T, passages []Passage, retErr error, seenID *int) *Service {
	t.Helper()
	p := &stubProvider{vecs: [][]float32{{0.1, 0.2, 0.3}}}
	e := NewEmbedder(p, "m", noop.NewTracerProvider().Tracer("test"))
	r := &userCapturingRetriever{
		passages: passages,
		err:      retErr,
		seenID:   seenID,
	}
	return NewService(e, r, nil, stubFeedRepo{}, 5, 4000, 20,
		noop.NewTracerProvider().Tracer("test"))
}

// invokeHandler drives an HTTP request through the bridge handler
// with a request id already stamped on the context (so log
// assertions have something to find).
func invokeHandler(t *testing.T, h http.Handler, target string, userID int) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req = req.WithContext(reqctx.WithRequestID(req.Context(), "test-req-id"))
	if userID != 0 {
		req = httpCtxWithUser(req, userID)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// TestHTTPHandler_HappyPath pins the contract: an authenticated
// request with ?query= returns 200, the JSON envelope has the
// right shape, and the citations mirror the canned passages
// (feed_id + snippet).
func TestHTTPHandler_HappyPath(t *testing.T) {
	var seenID int
	svc := newStubService(t, []Passage{
		{FeedID: 7, Title: "Alpha", Description: "first feed", Score: 0.91},
		{FeedID: 11, Title: "Beta", Description: "", Score: 0.80},
	}, nil, &seenID)
	h := Handler(svc)

	rr := invokeHandler(t, h, "/internal/rag/retrieve?query=hello", 42)

	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, "application/json; charset=utf-8", rr.Header().Get("Content-Type"))
	assert.Equal(t, 42, seenID, "JWT user id handed to Retrieve")

	// Raw body sanity: citations must serialise as [] not null —
	// httpx rejects null for a list field.
	assert.Contains(t, rr.Body.String(), `"citations":[`)

	var body retrieveResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	require.Len(t, body.Citations, 2)

	assert.Equal(t, int64(7), body.Citations[0].FeedID)
	assert.Equal(t, "Alpha\n\nfirst feed", body.Citations[0].Snippet,
		"snippet mirrors ChunkText shape (Title + \\n\\n + Description)")

	assert.Equal(t, int64(11), body.Citations[1].FeedID)
	assert.Equal(t, "Beta", body.Citations[1].Snippet,
		"empty description renders title-only")

	assert.NotEmpty(t, body.ContextBlock)
	assert.Contains(t, body.ContextBlock, "[Feed 1]")
	assert.Contains(t, body.ContextBlock, "first feed")
}

// TestHTTPHandler_MissingUserReturns401 pins the defensive
// branch: a handler-only call (no middleware in front) with no
// user id on the context returns 401. Production traffic never
// hits this — middleware.NewAuth short-circuits first — but the
// guard keeps the handler independently safe for tests and
// future sub-router mountings.
func TestHTTPHandler_MissingUserReturns401(t *testing.T) {
	svc := newStubService(t, nil, nil, nil)
	h := Handler(svc)

	rr := invokeHandler(t, h, "/internal/rag/retrieve?query=hello", 0)

	require.Equal(t, http.StatusUnauthorized, rr.Code)
	// Body shape matches the standard envelope; err.Error() never leaks.
	var body map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, "Authentication required", body["error"])
	assert.EqualValues(t, 401, body["code"])
}

// TestHTTPHandler_MissingQueryReturns400 pins that a request
// without ?query= returns 400 with a static safe detail. err.Error()
// never reaches the wire.
func TestHTTPHandler_MissingQueryReturns400(t *testing.T) {
	svc := newStubService(t, nil, nil, nil)
	h := Handler(svc)

	rr := invokeHandler(t, h, "/internal/rag/retrieve", 42)

	require.Equal(t, http.StatusBadRequest, rr.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, safeDetailMissingQuery, body["error"])
	assert.EqualValues(t, 400, body["code"])
	// The response shape must be the static safe-detail envelope
	// only — no library / SQL / bcrypt / JWT error fragments
	// leaked via err.Error(). The bridge's wire contract: a 400
	// means "your query was malformed"; nothing else.
	raw := rr.Body.String()
	assert.NotContains(t, raw, "sql:") //nolint:misspell // pgx / sqlx error prefixes must never appear
	assert.NotContains(t, raw, "bcrypt")
	assert.NotContains(t, raw, "jwt")
}

// TestHTTPHandler_JWTUserIDWinsOverQueryParam pins the per-owner
// scoping contract: the query param user_id is ignored as
// authoritative. A request with ?user_id=999 but a JWT for user
// 42 must invoke Retrieve(ctx, 42, ...). This is the load-bearing
// guard against cross-user RAG leaks via the bridge.
func TestHTTPHandler_JWTUserIDWinsOverQueryParam(t *testing.T) {
	var seenID int
	svc := newStubService(t, nil, nil, &seenID)
	h := Handler(svc)

	rr := invokeHandler(t, h, "/internal/rag/retrieve?query=hello&user_id=999", 42)

	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, 42, seenID,
		"JWT user id (42) must win over query param user_id (999)")
}

// TestHTTPHandler_InternalErrorReturnsEmpty pins the
// fail-soft contract: when svc.Retrieve errors, the response is
// 200 with an empty envelope (Python treats empty as "no RAG")
// AND a log entry captures the wrapped error for operators.
// The response body must NEVER contain err.Error().
func TestHTTPHandler_InternalErrorReturnsEmpty(t *testing.T) {
	buf := captureLogger(t)

	var seenID int
	svc := newStubService(t, nil, errors.New("kaboom-internal"), &seenID)
	h := Handler(svc)

	rr := invokeHandler(t, h, "/internal/rag/retrieve?query=hello", 42)

	require.Equal(t, http.StatusOK, rr.Code)
	// citations must be `[]` not `null` — httpx rejects null.
	assert.Contains(t, rr.Body.String(), `"citations":[]`)

	var body retrieveResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, "", body.ContextBlock)
	assert.Empty(t, body.Citations)

	// err.Error() must NEVER appear on the wire; the safe-error
	// funnel logged it instead with the request id.
	raw := rr.Body.String()
	assert.False(t, strings.Contains(raw, "kaboom-internal"),
		"err.Error() must not reach the wire")

	logged := buf.String()
	assert.Contains(t, logged, "kaboom-internal",
		"underlying error must be logged via api.ClassifyAndLog")
	assert.Contains(t, logged, "test-req-id",
		"log entry must carry the request id")
}
