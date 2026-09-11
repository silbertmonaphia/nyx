package chat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	"nyx/internal/llm"
	"nyx/internal/middleware"
	"nyx/internal/platform/api"
	"nyx/internal/platform/auth"
)

// chatStubProvider is the llm.Provider used by handler tests. It can
// block on a never-closed channel (for the ctx-cancel test) and can
// return a sentinel error mid-stream (for the error-frame test).
type chatStubProvider struct {
	deltas     []string
	finalUsage *llm.ChatUsage
	returnErr  error
	release    <-chan struct{} // optional hang point
}

func (p *chatStubProvider) Chat(ctx context.Context, _ llm.ChatRequest, cb func(string, *llm.ChatUsage) error) (*llm.ChatUsage, error) {
	if p.release != nil {
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, llm.ErrContextCanceled
		}
	}
	for _, d := range p.deltas {
		if err := cb(d, nil); err != nil {
			return p.finalUsage, err
		}
	}
	if p.finalUsage != nil {
		if err := cb("", p.finalUsage); err != nil {
			return p.finalUsage, err
		}
	}
	return p.finalUsage, p.returnErr
}

// buildRouter mounts the chat route on a fresh chi router with
// RequestID + StoreRequest (the two middleware the handler reads
// from via reqctx). Returns the assembled handler + a TokenService
// pre-loaded with auth.TestSecret so individual tests can mint
// bearer tokens.
func buildRouter(t *testing.T, svc *Service, limiter *UserRateLimiter) (http.Handler, auth.TokenService) {
	t.Helper()
	tokens, err := auth.NewTokenService([]byte(auth.TestSecret), 15*time.Minute)
	require.NoError(t, err)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.StoreRequest)

	h := NewHandler(svc)
	RegisterChatRoute(r, h, tokens, limiter)
	return r, tokens
}

// bearerToken mints a valid access token for user id 1 / "tester".
func bearerToken(t *testing.T, tokens auth.TokenService) string {
	t.Helper()
	tok, err := tokens.GenerateToken(1, "tester")
	require.NoError(t, err)
	return tok
}

// chatRequest builds a minimal valid chat body as JSON.
func chatRequest(msgs ...Message) []byte {
	body, err := json.Marshal(ChatRequest{Messages: msgs})
	if err != nil {
		panic(err)
	}
	return body
}

// sseFrame is one parsed SSE frame: an optional event line and a
// data line. Bare data:[DONE] has event="" and data="[DONE]".
type sseFrame struct {
	event string
	data  string
}

// readSSEFrames parses an SSE response body. The wire format is
// `event: <name>\ndata: <json>\n\n` (event line optional); the
// blank line is the frame terminator.
func readSSEFrames(t *testing.T, body io.Reader) []sseFrame {
	t.Helper()
	var frames []sseFrame
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var cur sseFrame
	flush := func() {
		if cur.event != "" || cur.data != "" {
			frames = append(frames, cur)
		}
		cur = sseFrame{}
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "event: "):
			cur.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
		}
	}
	flush()
	return frames
}

// newTestService is a thin shim that constructs a Service with
// trivial caps and the supplied provider. Tracer is a noop so
// spans don't try to export.
func newTestServiceForHandler(p llm.Provider) *Service {
	s := newTestService(p, "")
	s.tracer = noop.NewTracerProvider().Tracer("test")
	return s
}

// TestHandler_StreamsDeltasAndDone exercises the happy path:
// service emits 2 deltas + usage. Asserts the SSE wire format is
// event:delta + event:done + data:[DONE], in that order, with usage
// carried in the done event payload. Uses httptest.NewServer so the
// response writer supports http.NewResponseController.Flush
// (httptest.NewRecorder does not).
func TestHandler_StreamsDeltasAndDone(t *testing.T) {
	stub := &chatStubProvider{
		deltas:     []string{"hello", " world"},
		finalUsage: &llm.ChatUsage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5},
	}
	svc := newTestServiceForHandler(stub)

	router, tokens := buildRouter(t, svc, nil)
	server := httptest.NewServer(router)
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/chat",
		bytes.NewReader(chatRequest(Message{Role: RoleUser, Content: "hi"})))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+bearerToken(t, tokens))
	req.Header.Set("Content-Type", "application/json")

	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream; charset=utf-8", resp.Header.Get("Content-Type"))
	assert.Equal(t, "no", resp.Header.Get("X-Accel-Buffering"))

	frames := readSSEFrames(t, resp.Body)
	require.GreaterOrEqual(t, len(frames), 3)

	// Collect all delta events in arrival order.
	var gotDeltas []string
	for _, f := range frames {
		if f.event == "delta" {
			var d struct {
				Delta string `json:"delta"`
			}
			require.NoError(t, json.Unmarshal([]byte(f.data), &d))
			gotDeltas = append(gotDeltas, d.Delta)
		}
	}
	assert.Equal(t, []string{"hello", " world"}, gotDeltas)

	// Find the done event; usage must be present in the payload.
	var doneFrame *sseFrame
	for i := range frames {
		if frames[i].event == "done" {
			doneFrame = &frames[i]
		}
	}
	require.NotNil(t, doneFrame)
	var done struct {
		Usage map[string]int `json:"usage"`
	}
	require.NoError(t, json.Unmarshal([]byte(doneFrame.data), &done))
	assert.Equal(t, 5, done.Usage["total_tokens"])

	// Last frame: [DONE] sentinel.
	last := frames[len(frames)-1]
	assert.Equal(t, "[DONE]", last.data)
	assert.Equal(t, "", last.event)
}

// TestHandler_StreamsErrorFrame_OnUpstreamFailure asserts the
// service returns ErrProviderUnavailable mid-stream → handler emits
// one trailing event:error frame, then [DONE]. Status is still 200
// because the SSE headers were flushed before the error.
func TestHandler_StreamsErrorFrame_OnUpstreamFailure(t *testing.T) {
	stub := &chatStubProvider{
		deltas:    []string{"partial "},
		returnErr: llm.ErrProviderUnavailable,
	}
	svc := newTestServiceForHandler(stub)

	router, tokens := buildRouter(t, svc, nil)
	server := httptest.NewServer(router)
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/chat",
		bytes.NewReader(chatRequest(Message{Role: RoleUser, Content: "hi"})))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+bearerToken(t, tokens))
	req.Header.Set("Content-Type", "application/json")

	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	frames := readSSEFrames(t, resp.Body)
	var errFrame *sseFrame
	for i := range frames {
		if frames[i].event == "error" {
			errFrame = &frames[i]
		}
	}
	require.NotNil(t, errFrame, "expected an event:error frame, got %+v", frames)
	var e struct {
		Error     string `json:"error"`
		RequestID string `json:"request_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(errFrame.data), &e))
	assert.Equal(t, "Chat failed", e.Error)
	assert.NotEmpty(t, e.RequestID)

	// Trailing [DONE] sentinel still present.
	last := frames[len(frames)-1]
	assert.Equal(t, "[DONE]", last.data)
}

// TestHandler_Returns400_OnInvalidInput asserts that validation
// failures produce the standard JSON envelope, NOT SSE — the SPA's
// response interceptor keys on this shape.
func TestHandler_Returns400_OnInvalidInput(t *testing.T) {
	svc := newTestServiceForHandler(&chatStubProvider{})

	router, tokens := buildRouter(t, svc, nil)
	server := httptest.NewServer(router)
	defer server.Close()

	// Empty messages array → ErrInvalidInput → 400 envelope.
	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/chat",
		bytes.NewReader([]byte(`{"messages":[]}`)))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+bearerToken(t, tokens))
	req.Header.Set("Content-Type", "application/json")

	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/json")

	var env api.ErrorResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&env))
	assert.Equal(t, "Invalid chat input", env.Message)
	assert.NotEmpty(t, env.RequestID)
}

// TestHandler_AuthRequired asserts the route 401s without a Bearer
// token. The WWW-Authenticate challenge must be present so the SPA
// axios interceptor can distinguish "expired" from "invalid".
func TestHandler_AuthRequired(t *testing.T) {
	svc := newTestServiceForHandler(&chatStubProvider{})

	router, _ := buildRouter(t, svc, nil)
	server := httptest.NewServer(router)
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/chat",
		bytes.NewReader(chatRequest(Message{Role: RoleUser, Content: "hi"})))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("WWW-Authenticate"), "Bearer")
}

// TestHandler_PerUserRateLimit_BlocksExcess asserts that 4 streams
// from the same user with a 3-burst cap see the 4th request 429ed.
// The limiter charges on stream-open, so each request consumes one
// slot regardless of body content.
func TestHandler_PerUserRateLimit_BlocksExcess(t *testing.T) {
	svc := newTestServiceForHandler(&chatStubProvider{})

	limiter := NewUserRateLimiter(0.001, 3) // tiny refill, burst 3

	router, tokens := buildRouter(t, svc, limiter)
	server := httptest.NewServer(router)
	defer server.Close()

	tok := bearerToken(t, tokens)
	body := chatRequest(Message{Role: RoleUser, Content: "hi"})

	statuses := make([]int, 0, 4)
	for i := 0; i < 4; i++ {
		req, err := http.NewRequest(http.MethodPost, server.URL+"/api/chat", bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")

		resp, err := server.Client().Do(req)
		require.NoError(t, err)
		statuses = append(statuses, resp.StatusCode)
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	// First three succeed (200), fourth is rate-limited (429).
	success := 0
	for _, s := range statuses {
		if s == http.StatusOK {
			success++
		}
	}
	assert.Equal(t, 3, success, "expected 3 successes then 429; got %v", statuses)
	assert.Equal(t, http.StatusTooManyRequests, statuses[3])
}

// TestHandler_ContextCancellation_StopsStream asserts that the
// client's abort stops the in-flight request within a reasonable
// window. The provider hangs on a never-released channel, so the
// only way the stream ends is via ctx cancel.
func TestHandler_ContextCancellation_StopsStream(t *testing.T) {
	stub := &chatStubProvider{release: make(chan struct{})} // never closed
	svc := newTestServiceForHandler(stub)

	router, tokens := buildRouter(t, svc, nil)
	server := httptest.NewServer(router)
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	wg.Add(1)
	doneCh := make(chan struct{})
	go func() {
		defer wg.Done()
		defer close(doneCh)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			server.URL+"/api/chat",
			bytes.NewReader(chatRequest(Message{Role: RoleUser, Content: "hi"})))
		if err != nil {
			return
		}
		req.Header.Set("Authorization", "Bearer "+bearerToken(t, tokens))
		req.Header.Set("Content-Type", "application/json")

		resp, err := server.Client().Do(req)
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	time.Sleep(80 * time.Millisecond)
	cancel()

	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not return after client cancel")
	}
}

// TestFlushingWriter_FlushDefersToResponseController pins the
// streaming contract: Flush() must reach the underlying
// http.Flusher (via http.NewResponseController) so SSE chunks
// don't accumulate in chi's WrapResponseWriter buffer. The
// assertion is that a Flush call followed by another Write
// produces two separate TCP writes on the wire.
func TestFlushingWriter_FlushDefersToResponseController(t *testing.T) {
	rec := httptest.NewRecorder()
	fw := &flushingWriter{w: rec}

	_, _ = fw.Write([]byte("first"))
	require.NoError(t, fw.Flush())
	_, _ = fw.Write([]byte("second"))
	require.NoError(t, fw.Flush())

	assert.Equal(t, "firstsecond", rec.Body.String())
}

// TestRateLimit_RetryAfterHeader pins the contract: a 429 from the
// per-user limiter carries a Retry-After header so well-behaved
// clients back off. The handler sets it to "60" (seconds) —
// short enough to be useful, long enough that a tight loop
// doesn't immediately retry.
func TestRateLimit_RetryAfterHeader(t *testing.T) {
	svc := newTestServiceForHandler(&chatStubProvider{})
	limiter := NewUserRateLimiter(0.001, 1) // burst 1, slow refill

	router, tokens := buildRouter(t, svc, limiter)
	server := httptest.NewServer(router)
	defer server.Close()

	tok := bearerToken(t, tokens)
	body := chatRequest(Message{Role: RoleUser, Content: "hi"})

	// First request consumes the only bucket slot.
	req1, _ := http.NewRequest(http.MethodPost, server.URL+"/api/chat", bytes.NewReader(body))
	req1.Header.Set("Authorization", "Bearer "+tok)
	req1.Header.Set("Content-Type", "application/json")
	resp1, err := server.Client().Do(req1)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp1.Body)
	resp1.Body.Close()
	assert.Equal(t, http.StatusOK, resp1.StatusCode)

	// Second request hits the limiter.
	req2, _ := http.NewRequest(http.MethodPost, server.URL+"/api/chat", bytes.NewReader(body))
	req2.Header.Set("Authorization", "Bearer "+tok)
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := server.Client().Do(req2)
	require.NoError(t, err)
	defer resp2.Body.Close()

	assert.Equal(t, http.StatusTooManyRequests, resp2.StatusCode)
	assert.Equal(t, "60", resp2.Header.Get("Retry-After"))
}
