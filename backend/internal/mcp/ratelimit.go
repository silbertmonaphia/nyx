package mcp

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"nyx/internal/reqctx"
)

// userIDFromRequest reads the authenticated user ID stamped by the
// auth middleware. Returns 0 when the request never went through
// auth (which shouldn't happen in production — NewAuth runs first
// — but the limiter fails closed on 0 so a future route ordering
// regression can't silently let unauthenticated traffic past).
func userIDFromRequest(r *http.Request) int {
	return reqctx.UserIDFromContext(r.Context())
}

// writeRateLimitedResponse emits a JSON-RPC 2.0 error envelope
// (rather than the legacy {error,code,request_id,details} envelope
// the SPA uses) so an MCP agent receives a shape it can parse. The
// Retry-After header stays so HTTP-aware clients back off; the
// body also carries retry_after_seconds in the error.data field so
// agents that ignore headers still see the hint.
func writeRateLimitedResponse(w http.ResponseWriter, retryAfter time.Duration) {
	secs := int(retryAfter.Seconds())
	if secs < 1 {
		secs = 1
	}
	body, _ := json.Marshal(jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      nil,
		Error: &jsonRPCError{
			Code:    rpcAppRateLimited,
			Message: "Too many MCP requests",
			Data:    map[string]any{"retry_after_seconds": secs},
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "60")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write(body)
}

// UserRateLimiter is a per-user token bucket for MCP tool calls.
// In-memory only, no Redis, no shared state across instances. The
// cost of an in-process bucket is that a horizontally-scaled
// deployment effectively multiplies the cap by the replica count
// — which is the right behaviour for a soft cap ("don't hammer
// the server") and wrong for a hard quota ("don't spend more than
// $X/day per user"). MCP is the former.
//
// The bucket charges on request, not per-token or per-tool: a
// single agent session that walks through list → get → update in
// quick succession is three slots, all of which is intentional. MCP
// tool calls are cheap relative to LLM chat streams so the default
// rate is 60/min — ten times the chat default.
type UserRateLimiter struct {
	mu      sync.Mutex
	buckets map[int]*userBucket
	rate    float64 // tokens added per second
	burst   float64 // maximum tokens per bucket
	now     func() time.Time
}

type userBucket struct {
	tokens float64
	last   time.Time
}

// NewUserRateLimiter builds a limiter that adds `rate` tokens per
// second up to `burst` tokens per user. Typical call from
// cmd/api/main.go: NewUserRateLimiter(60.0/60.0, 20) — 60 calls
// per minute with a burst of 20, so a user can fan out 20 in quick
// succession and then is paced to 1/second.
func NewUserRateLimiter(rate, burst float64) *UserRateLimiter {
	return &UserRateLimiter{
		buckets: make(map[int]*userBucket),
		rate:    rate,
		burst:   burst,
		now:     time.Now,
	}
}

// Allow reports whether userID may issue a tool call right now.
// Replenishment is lazy: a user who never returns keeps their
// bucket forever, but the map's size is bounded by the active-user
// count which is itself bounded by the auth layer. A background
// GC (gcStale) trims buckets that haven't been touched in over
// an hour to bound memory under churn.
func (l *UserRateLimiter) Allow(userID int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, ok := l.buckets[userID]
	if !ok {
		b = &userBucket{tokens: l.burst, last: now}
		l.buckets[userID] = b
	}

	elapsed := now.Sub(b.last).Seconds()
	b.tokens += elapsed * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// gcStale drops buckets that haven't been touched in olderThan. The
// loop in main.go calls this on a ticker to bound memory under
// user-id churn.
func (l *UserRateLimiter) gcStale(olderThan time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := l.now().Add(-olderThan)
	for id, b := range l.buckets {
		if b.last.Before(cutoff) {
			delete(l.buckets, id)
		}
	}
}

// RunGC runs gcStale on the supplied interval until the returned
// stop function is called. Safe to call once per limiter; the
// goroutine exits on stop or when the channel it reads from is
// closed. The default from main.go is every minute, evicting
// buckets idle for over an hour.
func (l *UserRateLimiter) RunGC(interval, idleThreshold time.Duration) func() {
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				l.gcStale(idleThreshold)
			}
		}
	}()
	return func() { close(stop) }
}

// Middleware returns a chi middleware that emits a JSON-RPC error
// (rate-limited) when the authenticated user has no remaining
// bucket tokens. The middleware assumes NewAuth has already stamped
// the user id on the context; a 401 from NewAuth earlier in the
// chain prevents this middleware from running, so we never see
// userID == 0 in production.
func (l *UserRateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID := userIDFromRequest(r)
		if userID == 0 {
			// Auth should have rejected before us; if not, fail
			// closed rather than letting a 0-ID past the limiter.
			next.ServeHTTP(w, r)
			return
		}
		if !l.Allow(userID) {
			retryAfter := time.Duration(float64(time.Second) / l.rate)
			writeRateLimitedResponse(w, retryAfter)
			return
		}
		next.ServeHTTP(w, r)
	})
}
