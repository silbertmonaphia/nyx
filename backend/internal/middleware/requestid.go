package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	RequestIDHeader = "X-Request-ID"
	RequestIDKey    = "requestID"
)

// RequestID returns a middleware that injects a unique request ID into the request headers and context.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Check for existing request ID in headers
		requestID := c.GetHeader(RequestIDHeader)

		// If no request ID exists, generate a new one
		if requestID == "" {
			requestID = uuid.New().String()
		}

		// Set request ID in context and response headers
		c.Set(RequestIDKey, requestID)
		c.Header(RequestIDHeader, requestID)

		c.Next()
	}
}

// GetRequestID returns the request ID from the gin context.
func GetRequestID(c *gin.Context) string {
	if val, ok := c.Get(RequestIDKey); ok {
		if id, ok := val.(string); ok {
			return id
		}
	}
	return ""
}
