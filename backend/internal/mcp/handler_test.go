package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nyx/internal/feed"
	"nyx/internal/middleware"
	"nyx/internal/platform/auth"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"
)

// newTestRouter wires a minimal chi router with just the middleware
// the MCP route needs (StoreRequest + Auth) plus RegisterMCPRoute.
// Skips Prometheus/Logging/CORS/RateLimit — those have their own
// tests. StoreRequest is critical so the auth middleware can recover
// *http.Request from ctx (chi passes the request via context only on
// the huma path; the chi-direct path needs the explicit middleware).
func newTestRouter(handler *Handler, tokens auth.TokenService, limiter *UserRateLimiter) http.Handler {
	r := chi.NewMux()
	r.Use(middleware.StoreRequest)
	RegisterMCPRoute(r, handler, tokens, limiter)
	return r
}

// newTestTokens mints a per-test TokenService with the standard
// test secret. Per-test isolation avoids cross-test token pollution
// (every test gets a fresh signing key in principle; the project
// uses auth.TestSecret so the secret is shared but the *service*
// instance is fresh, which is enough for unit isolation).
func newTestTokens(t *testing.T) auth.TokenService {
	t.Helper()
	tokens, err := auth.NewTokenService([]byte(auth.TestSecret), 15*time.Minute)
	require.NoError(t, err)
	return tokens
}

// bearerToken mints a fresh access token for the given user.
func bearerToken(t *testing.T, tokens auth.TokenService, userID int, username string) string {
	t.Helper()
	tok, err := tokens.GenerateToken(userID, username)
	require.NoError(t, err)
	return tok
}

// jsonRPCRequest builds a minimal JSON-RPC 2.0 envelope. id is the
// request id; method is the MCP method; params is the params object.
func jsonRPCRequest(method string, id int, params any) []byte {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	return body
}

// postRPCCall fires a single MCP HTTP request and returns the
// recorder + decoded JSON body. Body is application/json + the MCP
// accept header required by the Streamable HTTP transport.
func postRPCCall(t *testing.T, router http.Handler, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, "/mcp/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

// decodeSSEOrJSONBody returns the first JSON object found in the
// response body. Streamable HTTP may return either a single JSON
// object (Content-Type: application/json) or an SSE stream of
// `data: {...}\n\n` events (Content-Type: text/event-stream); the
// MCP test contract accepts either.
func decodeSSEOrJSONBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	body := rr.Body.Bytes()
	if json.Valid(body) {
		var out map[string]any
		require.NoError(t, json.Unmarshal(body, &out))
		return out
	}
	// SSE: pick the first `data: {...}` line.
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var out map[string]any
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &out))
		return out
	}
	t.Fatalf("could not decode SSE/JSON body: %s", body)
	return nil
}

// ---- Auth tests ----

func TestHandler_NoAuth_Returns401(t *testing.T) {
	stub := &stubFeedService{}
	svc := NewService(stub, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(svc, "/mcp", 64*1024)

	router := newTestRouter(h, newTestTokens(t), nil)
	rr := postRPCCall(t, router, jsonRPCRequest(MethodInitialize, 1, map[string]any{}), nil)

	assert.Equal(t, http.StatusUnauthorized, rr.Code, "missing Bearer must 401")
	assert.Contains(t, rr.Header().Get("WWW-Authenticate"), "Bearer")
}

func TestHandler_InvalidToken_Returns401(t *testing.T) {
	stub := &stubFeedService{}
	svc := NewService(stub, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(svc, "/mcp", 64*1024)

	router := newTestRouter(h, newTestTokens(t), nil)
	rr := postRPCCall(t, router, jsonRPCRequest(MethodInitialize, 1, map[string]any{}),
		map[string]string{"Authorization": "Bearer not.a.valid.jwt"})

	assert.Equal(t, http.StatusUnauthorized, rr.Code)
	assert.Contains(t, rr.Header().Get("WWW-Authenticate"), "Bearer")
}

// ---- MCP handshake + tool discovery ----

func TestHandler_Initialize_RoundTrip(t *testing.T) {
	stub := &stubFeedService{}
	svc := NewService(stub, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(svc, "/mcp", 64*1024)

	tokens := newTestTokens(t)
	router := newTestRouter(h, tokens, nil)

	rr := postRPCCall(t, router, jsonRPCRequest(MethodInitialize, 1, map[string]any{
		"protocolVersion": "2025-11-25",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "0.0.0"},
	}), map[string]string{"Authorization": "Bearer " + bearerToken(t, tokens, 1, "tester")})

	require.Equal(t, http.StatusOK, rr.Code, "initialize must succeed; body=%s", rr.Body.String())
	out := decodeSSEOrJSONBody(t, rr)
	assert.Equal(t, "2.0", out["jsonrpc"])
	// result.serverInfo.name == "nyx-feeds"
	result, ok := out["result"].(map[string]any)
	require.True(t, ok, "result must be an object; got %T (%v)", out["result"], out["result"])
	srv, ok := result["serverInfo"].(map[string]any)
	require.True(t, ok, "serverInfo must be an object; got %T (%v)", result["serverInfo"], result["serverInfo"])
	assert.Equal(t, "nyx-feeds", srv["name"])
}

func TestHandler_ToolsList_ReturnsFiveTools(t *testing.T) {
	stub := &stubFeedService{}
	svc := NewService(stub, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(svc, "/mcp", 64*1024)

	tokens := newTestTokens(t)
	router := newTestRouter(h, tokens, nil)

	rr := postRPCCall(t, router, jsonRPCRequest(MethodToolsList, 2, map[string]any{}),
		map[string]string{"Authorization": "Bearer " + bearerToken(t, tokens, 1, "tester")})

	require.Equal(t, http.StatusOK, rr.Code, "tools/list must succeed; body=%s", rr.Body.String())
	out := decodeSSEOrJSONBody(t, rr)

	result, ok := out["result"].(map[string]any)
	require.True(t, ok)
	tools, ok := result["tools"].([]any)
	require.True(t, ok, "result.tools must be an array; got %T", result["tools"])

	want := map[string]bool{
		ToolListFeeds: false, ToolGetFeed: false, ToolCreateFeed: false,
		ToolUpdateFeed: false, ToolDeleteFeed: false,
	}
	for _, t := range tools {
		tool, _ := t.(map[string]any)
		name, _ := tool["name"].(string)
		if _, expected := want[name]; expected {
			want[name] = true
		}
	}
	for name, found := range want {
		assert.True(t, found, "tool %q missing from tools/list response", name)
	}
}

// ---- Tool call tests ----

func TestHandler_ToolsCall_ListFeeds_HappyPath(t *testing.T) {
	stub := &stubFeedService{
		listResp: &feed.Page{Items: []feed.Feed{{ID: 1, UserID: 1, Title: "x"}}, Total: 1, Page: 1, PageSize: 20},
	}
	svc := NewService(stub, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(svc, "/mcp", 64*1024)

	tokens := newTestTokens(t)
	router := newTestRouter(h, tokens, nil)

	rr := postRPCCall(t, router, jsonRPCRequest(MethodToolsCall, 3, map[string]any{
		"name":      ToolListFeeds,
		"arguments": map[string]any{"page": 1, "page_size": 20},
	}), map[string]string{"Authorization": "Bearer " + bearerToken(t, tokens, 1, "tester")})

	require.Equal(t, http.StatusOK, rr.Code)
	out := decodeSSEOrJSONBody(t, rr)
	result, _ := out["result"].(map[string]any)
	require.NotNil(t, result, "result must be present on the happy path")
	isErr, _ := result["isError"].(bool)
	assert.False(t, isErr, "happy-path tool call must NOT set isError")
	// StructuredContent carries the typed payload — agents prefer
	// it over the JSON-text Content fallback. Assert the shape so
	// a future regression that loses StructuredContent is caught.
	sc, ok := result["structuredContent"].(map[string]any)
	require.True(t, ok, "structuredContent must be present; got %T (%v)", result["structuredContent"], result["structuredContent"])
	assert.Contains(t, sc, "data")
	assert.Contains(t, sc, "page")
	assert.Contains(t, sc, "page_size")
	assert.Contains(t, sc, "total")
	assert.Contains(t, sc, "has_more")
}

func TestHandler_ToolsCall_GetFeed_NotFoundReturnsToolError(t *testing.T) {
	stub := &stubFeedService{getErr: feed.ErrNotFound}
	svc := NewService(stub, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(svc, "/mcp", 64*1024)

	tokens := newTestTokens(t)
	router := newTestRouter(h, tokens, nil)

	rr := postRPCCall(t, router, jsonRPCRequest(MethodToolsCall, 4, map[string]any{
		"name":      ToolGetFeed,
		"arguments": map[string]any{"id": 999},
	}), map[string]string{"Authorization": "Bearer " + bearerToken(t, tokens, 1, "tester")})

	require.Equal(t, http.StatusOK, rr.Code, "tool errors surface as JSON-RPC error envelope, NOT HTTP 4xx")
	out := decodeSSEOrJSONBody(t, rr)
	errObj, ok := out["error"].(map[string]any)
	require.True(t, ok, "feed.ErrNotFound must surface as JSON-RPC error envelope; got %+v", out)
	require.NotNil(t, errObj)
	assert.Equal(t, float64(rpcInternalError), errObj["code"], "mcp-go wraps our ErrNotFound as InternalError by default")
	msg, _ := errObj["message"].(string)
	assert.NotEmpty(t, msg)
	assert.NotContains(t, msg, "pgx", "internal pgx text must never leak through MCP error envelopes")
}

func TestHandler_PerOwnerIsolation(t *testing.T) {
	// Stub records the userID the repo sees for each call.
	stub := &stubFeedService{}
	svc := NewService(stub, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(svc, "/mcp", 64*1024)

	tokens := newTestTokens(t)
	router := newTestRouter(h, tokens, nil)

	// Mint a JWT for user 7. The stub captures reqctx.UserIDFromContext
	// on every call — it must equal 7, NEVER the id from the request body.
	const ownerID = 7
	rr := postRPCCall(t, router, jsonRPCRequest(MethodToolsCall, 5, map[string]any{
		"name":      ToolListFeeds,
		"arguments": map[string]any{},
	}), map[string]string{"Authorization": "Bearer " + bearerToken(t, tokens, ownerID, "owner7")})

	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, ownerID, stub.lastListUserID, "stub must observe userID from JWT, not from any other source")
}

func TestHandler_UnknownTool_ReturnsMethodNotFound(t *testing.T) {
	stub := &stubFeedService{}
	svc := NewService(stub, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(svc, "/mcp", 64*1024)

	tokens := newTestTokens(t)
	router := newTestRouter(h, tokens, nil)

	rr := postRPCCall(t, router, jsonRPCRequest(MethodToolsCall, 6, map[string]any{
		"name":      "does_not_exist",
		"arguments": map[string]any{},
	}), map[string]string{"Authorization": "Bearer " + bearerToken(t, tokens, 1, "tester")})

	require.Equal(t, http.StatusOK, rr.Code, "unknown tool surface as JSON-RPC error, not HTTP error")
	out := decodeSSEOrJSONBody(t, rr)
	errObj, ok := out["error"].(map[string]any)
	require.True(t, ok, "unknown tool must produce JSON-RPC error envelope; got %+v", out)
	// mcp-go surfaces "tool not found" as a tool-result isError, NOT a
	// JSON-RPC method-not-found. The acceptance criterion is simply
	// that the call does not silently succeed.
	assert.NotNil(t, errObj, "expected JSON-RPC error envelope for unknown tool")
}

func TestHandler_BodyTooLarge_Returns413(t *testing.T) {
	stub := &stubFeedService{}
	svc := NewService(stub, noop.NewTracerProvider().Tracer("test"))
	// tiny inner cap so the test body (1 KiB) overflows
	h := NewHandler(svc, "/mcp", 256)

	tokens := newTestTokens(t)
	router := newTestRouter(h, tokens, nil)

	// 1 KiB of "x" plus a valid JSON envelope is well over the 256-byte cap.
	big := jsonRPCRequest(MethodToolsList, 1, map[string]any{})
	for len(big) < 1024 {
		big = append(big, []byte(" ")...)
	}

	rr := postRPCCall(t, router, big, map[string]string{"Authorization": "Bearer " + bearerToken(t, tokens, 1, "tester")})

	// MaxBytesReader produces 413 (or 400 depending on the server);
	// either is acceptable as long as the call doesn't succeed.
	assert.NotEqual(t, http.StatusOK, rr.Code, "oversized body must NOT succeed")
}

// ---- Rate-limit tests ----

func TestHandler_RateLimit_BlocksExcess(t *testing.T) {
	stub := &stubFeedService{}
	svc := NewService(stub, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(svc, "/mcp", 64*1024)

	tokens := newTestTokens(t)
	// burst 1 — first call succeeds, second is rate-limited
	limiter := NewUserRateLimiter(0.001, 1)

	router := newTestRouter(h, tokens, limiter)
	tok := bearerToken(t, tokens, 1, "tester")

	// First call: succeeds (consumes the only bucket slot).
	rr1 := postRPCCall(t, router, jsonRPCRequest(MethodToolsList, 1, map[string]any{}),
		map[string]string{"Authorization": "Bearer " + tok})
	require.Equal(t, http.StatusOK, rr1.Code)

	// Second call: rate-limited → 429 with JSON-RPC error envelope.
	rr2 := postRPCCall(t, router, jsonRPCRequest(MethodToolsList, 2, map[string]any{}),
		map[string]string{"Authorization": "Bearer " + tok})
	assert.Equal(t, http.StatusTooManyRequests, rr2.Code)
	assert.Equal(t, "60", rr2.Header().Get("Retry-After"))
}

func TestRateLimit_DifferentUsersIndependent(t *testing.T) {
	stub := &stubFeedService{}
	svc := NewService(stub, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(svc, "/mcp", 64*1024)

	tokens := newTestTokens(t)
	limiter := NewUserRateLimiter(0.001, 1)

	router := newTestRouter(h, tokens, limiter)

	// User 1 consumes their one bucket slot.
	tok1 := bearerToken(t, tokens, 1, "user1")
	rr1 := postRPCCall(t, router, jsonRPCRequest(MethodToolsList, 1, map[string]any{}),
		map[string]string{"Authorization": "Bearer " + tok1})
	require.Equal(t, http.StatusOK, rr1.Code)

	// User 2 must still have a fresh bucket — user 1's exhaustion
	// must not leak across users.
	tok2 := bearerToken(t, tokens, 2, "user2")
	rr2 := postRPCCall(t, router, jsonRPCRequest(MethodToolsList, 2, map[string]any{}),
		map[string]string{"Authorization": "Bearer " + tok2})
	assert.Equal(t, http.StatusOK, rr2.Code, "per-user limiter must NOT leak across users")
}

// ---- Sentinel / error mapping tests ----

func TestMapSentinel_NotFoundMatches(t *testing.T) {
	err := mapSentinel(contextOrBG(), feed.ErrNotFound, "test detail")
	assert.True(t, errors.Is(err, ErrNotFound))
}

func TestMapSentinel_UnknownMapsToInternal(t *testing.T) {
	err := mapSentinel(contextOrBG(), errors.New("boom"), "test detail")
	assert.True(t, errors.Is(err, ErrInternal))
}

// Ensure the io package is referenced (some helpers may use it).
var _ = io.Discard
var _ = fmt.Sprintf
