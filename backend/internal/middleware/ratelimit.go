package middleware

import (
	"net/http"
	"sync"
	"time"

	"nyx/internal/platform/api"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

// RateLimiterConfig holds the configuration for rate limiting
type RateLimiterConfig struct {
	RequestsPerSecond float64 // Token bucket refill rate
	BurstSize         int     // Maximum burst size (bucket capacity)
}

// visitor holds the rate limiter and last access time for a client
type visitor struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// RateLimiter manages per-client rate limiters
type RateLimiter struct {
	visitors map[string]*visitor
	mu       sync.RWMutex
	config   RateLimiterConfig
}

// NewRateLimiter creates a new rate limiter with the given configuration
func NewRateLimiter(config RateLimiterConfig) *RateLimiter {
	return &RateLimiter{
		visitors: make(map[string]*visitor),
		config:   config,
	}
}

// getLimiter returns the rate limiter for a specific client IP
// Creates a new limiter if one doesn't exist for the client
func (rl *RateLimiter) getLimiter(clientIP string) *rate.Limiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	v, exists := rl.visitors[clientIP]
	if !exists {
		limiter := rate.NewLimiter(rate.Limit(rl.config.RequestsPerSecond), rl.config.BurstSize)
		rl.visitors[clientIP] = &visitor{limiter: limiter, lastSeen: time.Now()}
		return limiter
	}

	// Update last seen time
	v.lastSeen = time.Now()
	return v.limiter
}

// Cleanup removes stale visitors (not seen for more than 1 minute)
// Should be called periodically in a goroutine
func (rl *RateLimiter) Cleanup() {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	for ip, v := range rl.visitors {
		if time.Since(v.lastSeen) > time.Minute {
			delete(rl.visitors, ip)
		}
	}
}

// RateLimit creates a middleware that limits requests per client IP
// Uses token bucket algorithm for smooth rate limiting
func RateLimit(config RateLimiterConfig) gin.HandlerFunc {
	limiter := NewRateLimiter(config)

	// Start cleanup goroutine
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			limiter.Cleanup()
		}
	}()

	return func(c *gin.Context) {
		clientIP := c.ClientIP()
		lim := limiter.getLimiter(clientIP)

		if !lim.Allow() {
			api.AbortWithError(c, http.StatusTooManyRequests, "Rate limit exceeded. Please try again later.", nil)
			return
		}

		c.Next()
	}
}

// DefaultRateLimit provides sensible defaults: 10 req/s with burst of 20
func DefaultRateLimit() gin.HandlerFunc {
	return RateLimit(RateLimiterConfig{
		RequestsPerSecond: 10,
		BurstSize:         20,
	})
}
