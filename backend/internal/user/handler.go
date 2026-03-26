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

// Register adds a new user to the system
// @Summary Register a user
// @Description Create a new user account
// @Tags auth
// @Accept json
// @Produce json
// @Param request body RegisterRequest true "Registration details"
// @Success 201 {object} AuthResponse
// @Failure 400 {object} api.ErrorResponse
// @Failure 500 {object} api.ErrorResponse
// @Router /register [post]
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

// Login authenticates a user and returns a token
// @Summary Login a user
// @Description Authenticate a user and return a JWT token
// @Tags auth
// @Accept json
// @Produce json
// @Param request body LoginRequest true "Login credentials"
// @Success 200 {object} AuthResponse
// @Failure 400 {object} api.ErrorResponse
// @Failure 401 {object} api.ErrorResponse
// @Failure 500 {object} api.ErrorResponse
// @Router /login [post]
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
