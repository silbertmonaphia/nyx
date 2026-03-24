package api

import (
	"github.com/gin-gonic/gin"
)

// ErrorResponse defines the standard JSON structure for all API errors
type ErrorResponse struct {
	Error   string      `json:"error"`
	Code    int         `json:"code"`
	Details interface{} `json:"details,omitempty"`
}

// AbortWithError is a helper to send a standardized error response and abort the request
func AbortWithError(c *gin.Context, statusCode int, message string, details interface{}) {
	c.AbortWithStatusJSON(statusCode, ErrorResponse{
		Error:   message,
		Code:    statusCode,
		Details: details,
	})
}

// SendError is a helper to send a standardized error response without aborting (if needed)
func SendError(c *gin.Context, statusCode int, message string, details interface{}) {
	c.JSON(statusCode, ErrorResponse{
		Error:   message,
		Code:    statusCode,
		Details: details,
	})
}
