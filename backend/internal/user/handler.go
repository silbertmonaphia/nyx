package user

import (
	"errors"
	"net/http"
	"nyx/internal/platform/api"

	"github.com/gin-gonic/gin"
)

type Handler struct {
	service Service
}

func NewHandler(service Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) Register(c *gin.Context) {
	var req RegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.AbortWithError(c, http.StatusBadRequest, "Invalid registration request", err.Error())
		return
	}

	res, err := h.service.Register(c.Request.Context(), req)
	if err != nil {
		api.AbortWithError(c, http.StatusInternalServerError, "Failed to register user", err.Error())
		return
	}

	c.JSON(http.StatusCreated, res)
}

func (h *Handler) Login(c *gin.Context) {
	var req LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.AbortWithError(c, http.StatusBadRequest, "Invalid login request", err.Error())
		return
	}

	res, err := h.service.Login(c.Request.Context(), req)
	if err != nil {
		if errors.Is(err, ErrInvalidCredentials) {
			api.AbortWithError(c, http.StatusUnauthorized, "Invalid credentials", nil)
			return
		}
		api.AbortWithError(c, http.StatusInternalServerError, "Failed to login", err.Error())
		return
	}

	c.JSON(http.StatusOK, res)
}
