package movie

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, movies)
}

func (h *Handler) CreateMovieHandler(c *gin.Context) {
	var m Movie
	if err := c.ShouldBindJSON(&m); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request payload", "details": err.Error()})
		return
	}

	if err := h.service.CreateMovie(c.Request.Context(), &m); err != nil {
		log.Error().Err(err).Msg("Error inserting movie")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
		return
	}

	c.JSON(http.StatusCreated, m)
}

func (h *Handler) UpdateMovieHandler(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid movie ID"})
		return
	}

	var m Movie
	if err := c.ShouldBindJSON(&m); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request payload", "details": err.Error()})
		return
	}

	if err := h.service.UpdateMovie(c.Request.Context(), id, &m); err != nil {
		if err.Error() == "movie not found" {
			c.JSON(http.StatusNotFound, gin.H{"error": "Movie not found"})
			return
		}
		log.Error().Err(err).Msg("Error updating movie")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
		return
	}

	m.ID = id
	c.JSON(http.StatusOK, m)
}

func (h *Handler) DeleteMovieHandler(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid movie ID"})
		return
	}

	if err := h.service.DeleteMovie(c.Request.Context(), id); err != nil {
		if err.Error() == "movie not found" {
			c.JSON(http.StatusNotFound, gin.H{"error": "Movie not found"})
			return
		}
		log.Error().Err(err).Msg("Error deleting movie")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
		return
	}

	c.Status(http.StatusNoContent)
}
