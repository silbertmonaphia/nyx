package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestRateLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("allows requests within limit", func(t *testing.T) {
		limiter := RateLimit(RateLimiterConfig{
			RequestsPerSecond: 10,
			BurstSize:         5,
		})

		w := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(w)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/test", nil)

		limiter(ctx)

		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("blocks requests exceeding limit", func(t *testing.T) {
		// Very restrictive limit: 1 req/s with burst of 2
		limiter := RateLimit(RateLimiterConfig{
			RequestsPerSecond: 1,
			BurstSize:         2,
		})

		// First 2 requests should succeed (burst)
		for i := 0; i < 2; i++ {
			w := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(w)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/test", nil)
			// Simulate same client IP
			ctx.Request.RemoteAddr = "127.0.0.1:1234"

			limiter(ctx)
			assert.Equal(t, http.StatusOK, w.Code, "Request %d should succeed", i+1)
		}

		// Third request should be rate limited
		w := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(w)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/test", nil)
		ctx.Request.RemoteAddr = "127.0.0.1:1234"

		limiter(ctx)

		assert.Equal(t, http.StatusTooManyRequests, w.Code)
	})

	t.Run("different clients have separate limits", func(t *testing.T) {
		limiter := RateLimit(RateLimiterConfig{
			RequestsPerSecond: 1,
			BurstSize:         1,
		})

		// Client 1 uses their token
		w1 := httptest.NewRecorder()
		ctx1, _ := gin.CreateTestContext(w1)
		ctx1.Request = httptest.NewRequest(http.MethodGet, "/test", nil)
		ctx1.Request.RemoteAddr = "192.168.1.1:1234"
		limiter(ctx1)
		assert.Equal(t, http.StatusOK, w1.Code)

		// Client 2 should still have their token
		w2 := httptest.NewRecorder()
		ctx2, _ := gin.CreateTestContext(w2)
		ctx2.Request = httptest.NewRequest(http.MethodGet, "/test", nil)
		ctx2.Request.RemoteAddr = "192.168.1.2:1234"
		limiter(ctx2)
		assert.Equal(t, http.StatusOK, w2.Code)
	})
}

func TestRateLimiterCleanup(t *testing.T) {
	limiter := NewRateLimiter(RateLimiterConfig{
		RequestsPerSecond: 10,
		BurstSize:         20,
	})

	// Create a visitor
	_ = limiter.getLimiter("192.168.1.1")

	assert.Len(t, limiter.visitors, 1)

	// Manually set lastSeen to past
	limiter.mu.Lock()
	limiter.visitors["192.168.1.1"].lastSeen = time.Now().Add(-2 * time.Minute)
	limiter.mu.Unlock()

	// Run cleanup
	limiter.Cleanup()

	assert.Len(t, limiter.visitors, 0, "Stale visitors should be cleaned up")
}

func TestDefaultRateLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := DefaultRateLimit()

	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/test", nil)

	handler(ctx)

	assert.Equal(t, http.StatusOK, w.Code)
}
