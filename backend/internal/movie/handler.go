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

// HealthHandler checks the health of the API and database
// @Summary Check health
// @Description Check the status of the API and its database connection
// @Tags health
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Router /health [get]
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

// GetMoviesHandler retrieves a list of movies
// @Summary Get movies
// @Description Retrieve a list of movies, optionally filtered by title or description
// @Tags movies
// @Produce json
// @Param q query string false "Search query"
// @Success 200 {array} Movie
// @Failure 500 {object} api.ErrorResponse
// @Router /movies [get]
func (h *Handler) GetMoviesHandler(c *gin.Context) {
	queryParam := c.Query("q")
	movies, err := h.service.GetMovies(c.Request.Context(), queryParam)
	if err != nil {
		api.AbortWithError(c, http.StatusInternalServerError, "Failed to retrieve movies", err.Error())
		return
	}

	c.JSON(http.StatusOK, movies)
}

// CreateMovieHandler adds a new movie to the system
// @Summary Create a movie
// @Description Create a new movie record
// @Tags movies
// @Accept json
// @Produce json
// @Param movie body Movie true "Movie object"
// @Success 201 {object} Movie
// @Failure 400 {object} api.ErrorResponse
// @Failure 401 {object} api.ErrorResponse
// @Failure 500 {object} api.ErrorResponse
// @Security ApiKeyAuth
// @Router /movies [post]
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

// UpdateMovieHandler updates an existing movie record
// @Summary Update a movie
// @Description Update the details of an existing movie
// @Tags movies
// @Accept json
// @Produce json
// @Param id path int true "Movie ID"
// @Param movie body Movie true "Updated movie object"
// @Success 200 {object} Movie
// @Failure 400 {object} api.ErrorResponse
// @Failure 401 {object} api.ErrorResponse
// @Failure 404 {object} api.ErrorResponse
// @Failure 500 {object} api.ErrorResponse
// @Security ApiKeyAuth
// @Router /movies/{id} [put]
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

// DeleteMovieHandler removes a movie from the system
// @Summary Delete a movie
// @Description Remove a movie record by ID
// @Tags movies
// @Param id path int true "Movie ID"
// @Success 204 "No Content"
// @Failure 400 {object} api.ErrorResponse
// @Failure 401 {object} api.ErrorResponse
// @Failure 404 {object} api.ErrorResponse
// @Failure 500 {object} api.ErrorResponse
// @Security ApiKeyAuth
// @Router /movies/{id} [delete]
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
