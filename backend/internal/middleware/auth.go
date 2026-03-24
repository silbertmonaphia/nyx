package middleware

import (
	"net/http"
	"nyx/internal/platform/api"
	"nyx/internal/platform/auth"
	"strings"

	"github.com/gin-gonic/gin"
)

func Auth() gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			api.AbortWithError(c, http.StatusUnauthorized, "Authorization header is required", nil)
			return
		}

		parts := strings.Split(authHeader, " ")
		if len(parts) != 2 || parts[0] != "Bearer" {
			api.AbortWithError(c, http.StatusUnauthorized, "Authorization header must be in the format 'Bearer <token>'", nil)
			return
		}

		claims, err := auth.ValidateToken(parts[1])
		if err != nil {
			api.AbortWithError(c, http.StatusUnauthorized, "Invalid or expired token", err.Error())
			return
		}

		// Store user info in context
		c.Set("user_id", claims.UserID)
		c.Set("username", claims.Username)

		c.Next()
	}
}
