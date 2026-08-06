package middleware

import (
	"net/http"
	"sync"
	"time"

	"nyx/internal/platform/api"
	"nyx/internal/reqctx"

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
