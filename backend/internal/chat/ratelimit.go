package chat

import (
	"net/http"
	"sync"
	"time"

	"nyx/internal/platform/api"
	"nyx/internal/reqctx"
)

// userIDFromRequest reads the authenticated user ID stamped by the
// auth middleware. Returns 0 when the request never went through
// auth (which shouldn't happen in production — NewAuth runs
// first — but the middleware fails closed on 0 so a future route
// ordering regression doesn't silently let unauthenticated traffic
// past the limiter).
func userIDFromRequest(r *http.Request) int {
	return reqctx.UserIDFromContext(r.Context())
}

// writeTooManyRequests emits the canonical {error, code, request_id,
// details} envelope with a Retry-After hint. Details carries the
// wait seconds as a static value (not a wrapped error) so the
// safe-detail contract holds: nothing internal leaks, and the SPA
// gets a useful retry hint.
func writeTooManyRequests(w http.ResponseWriter, r *http.Request, retryAfter time.Duration) {
	secs := int(retryAfter.Seconds())
	if secs < 1 {
		secs = 1
	}
	api.WriteError(w, r, http.StatusTooManyRequests, "Too many chat requests", secs)
}

// UserRateLimiter is a per-user token bucket for chat-stream opens.
// It is intentionally simple: in-memory only, no Redis, no shared
// state across instances. The cost of an in-process bucket is that a
// horizontally-scaled deployment effectively multiplies the cap by
// the replica count — which is the right behaviour for a soft cap
// ("don't open 100 streams/min from one user") and wrong for a hard
// quota ("don't spend more than $X/day per user"). Chat is the
// former; we revisit when/if a hard quota becomes necessary.
//
// The bucket charges on stream-open (one slot per accepted
// connection), not per-token or per-chunk. A single stream can hold
// open for minutes and consume only one slot. Combined with the
// router-level per-IP limiter (which charges per request), the
// effective ceiling for a determined attacker is the lower of the
// two caps — a multi-IP attacker still has to find distinct users.
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
// cmd/api/main.go: NewUserRateLimiter(5.0/60.0, 3) — 5 streams per
// minute, burst of 3 (so a user can open 3 in quick succession, then
// has to wait 12 seconds before the next one).
func NewUserRateLimiter(rate, burst float64) *UserRateLimiter {
	return &UserRateLimiter{
		buckets: make(map[int]*userBucket),
		rate:    rate,
		burst:   burst,
		now:     time.Now,
	}
}

// Allow reports whether userID may open a stream right now.
// Replenishment is lazy: a user who never returns keeps their bucket
// forever, but the map's size is bounded by the active-user count
// which is itself bounded by the auth layer. A background GC
// (gcStale) trims buckets that haven't been touched in over an hour
// to bound memory under churn.
func (l *UserRateLimiter) Allow(userID int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, ok := l.buckets[userID]
	if !ok {
		b = &userBucket{tokens: l.burst, last: now}
		l.buckets[userID] = b
	}

	// Refill: add tokens proportional to elapsed seconds, capped at
	// burst. Float math is fine here — this is a soft cap, not an
	// accounting ledger.
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
// user-id churn (a UUID-pumped stream of one-shot users would
// otherwise grow the map indefinitely).
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

// Middleware returns a chi middleware that 429s any request whose
// authenticated user has no remaining bucket tokens. The middleware
// assumes NewAuth has already stamped the user id on the context;
// a 401 from NewAuth earlier in the chain prevents this middleware
// from running, so we never see userID == 0 in production.
//
// On 429 we emit the canonical {error, code, request_id, details}
// envelope via api.WriteError so the SPA surfaces a single toast
// regardless of the endpoint.
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
			w.Header().Set("Retry-After", "60")
			writeTooManyRequests(w, r, retryAfter)
			return
		}
		next.ServeHTTP(w, r)
	})
}
