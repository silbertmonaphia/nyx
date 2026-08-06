package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// okHandler is a stand-in downstream handler used by the rate-limit
// tests. Every request that passes the limiter reaches this handler
// and is answered with 200.
func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestRateLimit(t *testing.T) {
	t.Run("allows requests within limit", func(t *testing.T) {
		limiter := RateLimit(RateLimiterConfig{
			RequestsPerSecond: 10,
			BurstSize:         5,
		})
		handler := limiter(okHandler())

		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/test", nil)
		handler.ServeHTTP(w, r)

		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("blocks requests exceeding limit", func(t *testing.T) {
		// 1 req/s with burst 2 — third request inside the same
		// test should be throttled.
		limiter := RateLimit(RateLimiterConfig{
			RequestsPerSecond: 1,
			BurstSize:         2,
		})
		handler := limiter(okHandler())

		for i := 0; i < 2; i++ {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/test", nil)
			r.RemoteAddr = "127.0.0.1:1234"
			handler.ServeHTTP(w, r)
			assert.Equal(t, http.StatusOK, w.Code, "Request %d should succeed", i+1)
		}

		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/test", nil)
		r.RemoteAddr = "127.0.0.1:1234"
		handler.ServeHTTP(w, r)

		assert.Equal(t, http.StatusTooManyRequests, w.Code)
	})

	t.Run("different clients have separate limits", func(t *testing.T) {
		limiter := RateLimit(RateLimiterConfig{
			RequestsPerSecond: 1,
			BurstSize:         1,
		})
		handler := limiter(okHandler())

		w1 := httptest.NewRecorder()
		r1 := httptest.NewRequest(http.MethodGet, "/test", nil)
		r1.RemoteAddr = "192.168.1.1:1234"
		handler.ServeHTTP(w1, r1)
		assert.Equal(t, http.StatusOK, w1.Code)

		w2 := httptest.NewRecorder()
		r2 := httptest.NewRequest(http.MethodGet, "/test", nil)
		r2.RemoteAddr = "192.168.1.2:1234"
		handler.ServeHTTP(w2, r2)
		assert.Equal(t, http.StatusOK, w2.Code)
	})
}

func TestRateLimiterCleanup(t *testing.T) {
	limiter := NewRateLimiter(RateLimiterConfig{
		RequestsPerSecond: 10,
		BurstSize:         20,
	})

	_ = limiter.getLimiter("192.168.1.1")
	assert.Len(t, limiter.visitors, 1)

	limiter.mu.Lock()
	limiter.visitors["192.168.1.1"].lastSeen = time.Now().Add(-2 * time.Minute)
	limiter.mu.Unlock()

	limiter.Cleanup()
	assert.Len(t, limiter.visitors, 0, "Stale visitors should be cleaned up")
}

func TestDefaultRateLimit(t *testing.T) {
	handler := DefaultRateLimit()(okHandler())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/test", nil)
	handler.ServeHTTP(w, r)

	assert.Equal(t, http.StatusOK, w.Code)
}
