package movie

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"
	"nyx/internal/platform/api"
)

type Handler struct {
	service Service
}

func NewHandler(service Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) HealthHandler(c *gin.Context) {
	dbStatus := "up"
	if err := h.service.CheckHealth(c.Request.Context()); err != nil {
		dbStatus = "down"
		log.Error().Err(err).Msg("Database health check failed")
	}

	status := "ok"
	if dbStatus == "down" {
		status = "error"
	}

	c.JSON(http.StatusOK, gin.H{
		"status": status,
		"services": gin.H{
			"api":      "up",
			"database": dbStatus,
		},
	})
}

func (h *Handler) GetMoviesHandler(c *gin.Context) {
	queryParam := c.Query("q")
	movies, err := h.service.GetMovies(c.Request.Context(), queryParam)
	if err != nil {
		api.AbortWithError(c, http.StatusInternalServerError, "Failed to retrieve movies", err.Error())
		return
	}

	c.JSON(http.StatusOK, movies)
}

func (h *Handler) CreateMovieHandler(c *gin.Context) {
	var m Movie
	if err := c.ShouldBindJSON(&m); err != nil {
		api.AbortWithError(c, http.StatusBadRequest, "Invalid request payload", err.Error())
		return
	}

	if err := h.service.CreateMovie(c.Request.Context(), &m); err != nil {
		log.Error().Err(err).Msg("Error inserting movie")
		api.AbortWithError(c, http.StatusInternalServerError, "Database error", err.Error())
		return
	}

	c.JSON(http.StatusCreated, m)
}

func (h *Handler) UpdateMovieHandler(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		api.AbortWithError(c, http.StatusBadRequest, "Invalid movie ID", nil)
		return
	}

	var m Movie
	if err := c.ShouldBindJSON(&m); err != nil {
		api.AbortWithError(c, http.StatusBadRequest, "Invalid request payload", err.Error())
		return
	}

	if err := h.service.UpdateMovie(c.Request.Context(), id, &m); err != nil {
		if err.Error() == "movie not found" {
			api.AbortWithError(c, http.StatusNotFound, "Movie not found", nil)
			return
		}
		log.Error().Err(err).Msg("Error updating movie")
		api.AbortWithError(c, http.StatusInternalServerError, "Database error", err.Error())
		return
	}

	m.ID = id
	c.JSON(http.StatusOK, m)
}

func (h *Handler) DeleteMovieHandler(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		api.AbortWithError(c, http.StatusBadRequest, "Invalid movie ID", nil)
		return
	}

	if err := h.service.DeleteMovie(c.Request.Context(), id); err != nil {
		if err.Error() == "movie not found" {
			api.AbortWithError(c, http.StatusNotFound, "Movie not found", nil)
			return
		}
		log.Error().Err(err).Msg("Error deleting movie")
		api.AbortWithError(c, http.StatusInternalServerError, "Database error", err.Error())
		return
	}

	c.Status(http.StatusNoContent)
}
