package middleware

import (
	"net/http"
	"sync"
	"time"

	"nyx/internal/platform/api"
	"nyx/internal/reqctx"

	"github.com/danielgtaylor/huma/v2"
	"golang.org/x/time/rate"
)

// RateLimiterConfig holds the configuration for rate limiting. The
// rate.Limit type accepts fractional values; requests_per_second=0.5
// means one token every two seconds.
type RateLimiterConfig struct {
	RequestsPerSecond float64
	BurstSize         int
}

// visitor holds the per-client rate limiter and last-access time so
// the cleanup goroutine can drop idle entries.
type visitor struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// RateLimiter manages per-client rate limiters keyed by client IP.
// The implementation is identical to the gin-era version — the only
// surface that changed is the middleware signature.
type RateLimiter struct {
	visitors map[string]*visitor
	mu       sync.RWMutex
	config   RateLimiterConfig
}

// NewRateLimiter creates a new rate limiter with the given configuration.
func NewRateLimiter(config RateLimiterConfig) *RateLimiter {
	return &RateLimiter{
		visitors: make(map[string]*visitor),
		config:   config,
	}
}

// getLimiter returns the rate limiter for a specific client IP,
// creating one on first sight. lastSeen is updated so the cleanup
// goroutine doesn't drop an active visitor.
func (rl *RateLimiter) getLimiter(clientIP string) *rate.Limiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	v, exists := rl.visitors[clientIP]
	if !exists {
		limiter := rate.NewLimiter(rate.Limit(rl.config.RequestsPerSecond), rl.config.BurstSize)
		rl.visitors[clientIP] = &visitor{limiter: limiter, lastSeen: time.Now()}
		return limiter
	}

	v.lastSeen = time.Now()
	return v.limiter
}

// Cleanup removes visitors not seen for more than one minute. Should
// be called periodically from a goroutine (RateLimit starts one).
func (rl *RateLimiter) Cleanup() {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	for ip, v := range rl.visitors {
		if time.Since(v.lastSeen) > time.Minute {
			delete(rl.visitors, ip)
		}
	}
}

// RateLimit returns a middleware that limits requests per client IP
// using the token-bucket algorithm. The client IP is read from the
// context (populated by the RealIP middleware) and falls back to
// r.RemoteAddr if RealIP was not installed.
//
// TODO: the cleanup goroutine has no stop mechanism and outlives
// server.Shutdown's 5s window. Move to a ctx-cancellable constructor
// (signature: RateLimit(ctx, config) -> (middleware, stop)) and
// defer stop() in main.go. Deferred until the middleware lifecycle
// pattern is unified across the package — every existing limiter
// has the same issue.
func RateLimit(config RateLimiterConfig) func(http.Handler) http.Handler {
	limiter := NewRateLimiter(config)

	// Background cleanup of idle visitors. Stops when the process exits.
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			limiter.Cleanup()
		}
	}()

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			clientIP := reqctx.ClientIPFromContext(r.Context())
			if clientIP == "" {
				clientIP = reqctx.ClientIPFromRequest(r)
			}
			lim := limiter.getLimiter(clientIP)

			if !lim.Allow() {
				api.WriteError(w, r, http.StatusTooManyRequests, "Rate limit exceeded. Please try again later.", nil)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// DefaultRateLimit provides sensible defaults: 10 req/s with burst 20.
func DefaultRateLimit() func(http.Handler) http.Handler {
	return RateLimit(RateLimiterConfig{
		RequestsPerSecond: 10,
		BurstSize:         20,
	})
}

// AuthRateLimit returns a stricter huma.Middleware for the auth
// endpoints (/api/login, /api/register). It uses a fresh, isolated
// RateLimiter (not the global one) so an attacker hitting the
// login endpoint from one IP can't blow the global quota for
// every other route — and vice versa. Per-username lockout
// (post-failure-throttling) is NOT included here: it would require
// a separate shared state (Redis) for the multi-replica deployment
// Nyx plans for, and is tracked as a follow-up. For now the
// per-route IP limit is the credential-stuffing defence (see
// SECURITY.md M3).
func AuthRateLimit(config RateLimiterConfig) func(huma.Context, func(huma.Context)) {
	limiter := NewRateLimiter(config)

	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			limiter.Cleanup()
		}
	}()

	return func(ctx huma.Context, next func(huma.Context)) {
		clientIP := reqctx.ClientIPFromContext(ctx.Context())
		if clientIP == "" {
			if r := reqctx.RequestFromContext(ctx.Context()); r != nil {
				clientIP = reqctx.ClientIPFromRequest(r)
			}
		}
		if !limiter.getLimiter(clientIP).Allow() {
			writeHumaError(ctx, http.StatusTooManyRequests, "Too many attempts. Please try again later.", nil)
			return
		}
		next(ctx)
	}
}

// DefaultAuthRateLimit is the stricter default for /api/login and
// /api/register: 5 req/s with burst 10. Lets a real user retry a
// typo'd password within a second, but flattens a credential-
// stuffing flood.
func DefaultAuthRateLimit() func(huma.Context, func(huma.Context)) {
	return AuthRateLimit(RateLimiterConfig{
		RequestsPerSecond: 5,
		BurstSize:         10,
	})
}
