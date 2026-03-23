package main

import (
	"context"
	"database/sql"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
)

type Movie struct {
	ID          int        `json:"id"`
	Title       string     `json:"title" binding:"required,min=1,max=100"`
	Description string     `json:"description" binding:"max=1000"`
	Rating      float64    `json:"rating" binding:"min=0,max=10"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	DeletedAt   *time.Time `json:"deleted_at,omitempty"`
}

var db *sql.DB

func main() {
	// Configure zerolog
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = log.Output(os.Stdout)

	var err error
	dbURL := os.Getenv("DB_URL")
	if dbURL == "" {
		log.Fatal().Msg("DB_URL environment variable is required")
	}

	// Retry loop for database connection
	for i := 0; i < 10; i++ {
		db, err = sql.Open("postgres", dbURL)
		if err == nil {
			err = db.Ping()
			if err == nil {
				log.Info().Msg("Successfully connected to the database")
				break
			}
		}
		log.Warn().
			Int("attempt", i+1).
			Int("max_attempts", 10).
			Err(err).
			Msg("Could not connect to DB")
		time.Sleep(3 * time.Second)
	}

	if err != nil {
		log.Fatal().Err(err).Msg("Could not connect to database after retries")
	}
	defer db.Close()

	// Run database migrations
	runMigrations(dbURL)

	// Set up Gin
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()

	// Middleware
	router.Use(gin.Recovery())
	router.Use(loggingMiddleware())
	router.Use(corsMiddleware())

	// Routes
	api := router.Group("/api")
	{
		api.GET("/health", healthHandler)
		api.GET("/movies", getMoviesHandler)
		api.POST("/movies", createMovieHandler)
		api.PUT("/movies/:id", updateMovieHandler)
		api.DELETE("/movies/:id", deleteMovieHandler)
	}

	port := ":8080"
	server := &http.Server{
		Addr:    port,
		Handler: router,
	}

	// Create a cancellable context for handling signals
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Start server in a goroutine
	go func() {
		log.Info().Str("port", port).Msg("Server starting")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal().Err(err).Msg("Server failed to start")
		}
	}()

	// Wait for shutdown signal
	<-ctx.Done()

	// Graceful shutdown
	log.Info().Msg("Shutting down server gracefully")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Fatal().Err(err).Msg("Server shutdown failed")
	}

	log.Info().Msg("Server exited properly")
}

func runMigrations(dbURL string) {
	m, err := migrate.New(
		"file://migrations",
		dbURL,
	)
	if err != nil {
		log.Fatal().Err(err).Msg("Could not create migration instance")
	}

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		log.Fatal().Err(err).Msg("An error occurred while running migrations")
	}

	log.Info().Msg("Database migrations applied successfully")
}

func healthHandler(c *gin.Context) {
	dbStatus := "up"
	if err := db.Ping(); err != nil {
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

func getMoviesHandler(c *gin.Context) {
	queryParam := c.Query("q")
	var rows *sql.Rows
	var err error

	if queryParam != "" {
		rows, err = db.Query("SELECT id, title, description, rating, created_at, updated_at, deleted_at FROM movies WHERE title ILIKE $1 AND deleted_at IS NULL", "%"+queryParam+"%")
	} else {
		rows, err = db.Query("SELECT id, title, description, rating, created_at, updated_at, deleted_at FROM movies WHERE deleted_at IS NULL")
	}

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()

	var movies []Movie = []Movie{}
	for rows.Next() {
		var m Movie
		if err := rows.Scan(&m.ID, &m.Title, &m.Description, &m.Rating, &m.CreatedAt, &m.UpdatedAt, &m.DeletedAt); err != nil {
			log.Error().Err(err).Msg("Error scanning row")
			continue
		}
		movies = append(movies, m)
	}

	c.JSON(http.StatusOK, movies)
}

func createMovieHandler(c *gin.Context) {
	var m Movie
	if err := c.ShouldBindJSON(&m); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request payload", "details": err.Error()})
		return
	}

	query := "INSERT INTO movies (title, description, rating) VALUES ($1, $2, $3) RETURNING id, created_at, updated_at"
	err := db.QueryRow(query, m.Title, m.Description, m.Rating).Scan(&m.ID, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		log.Error().Err(err).Msg("Error inserting movie")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
		return
	}

	c.JSON(http.StatusCreated, m)
}

func updateMovieHandler(c *gin.Context) {
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

	query := "UPDATE movies SET title = $1, description = $2, rating = $3, updated_at = CURRENT_TIMESTAMP WHERE id = $4 AND deleted_at IS NULL RETURNING created_at, updated_at"
	err = db.QueryRow(query, m.Title, m.Description, m.Rating, id).Scan(&m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
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

func deleteMovieHandler(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid movie ID"})
		return
	}

	query := "UPDATE movies SET deleted_at = CURRENT_TIMESTAMP WHERE id = $1 AND deleted_at IS NULL"
	res, err := db.Exec(query, id)
	if err != nil {
		log.Error().Err(err).Msg("Error deleting movie")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
		return
	}

	rowsAffected, _ := res.RowsAffected()
	if rowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Movie not found"})
		return
	}

	c.Status(http.StatusNoContent)
}

// Middleware

func loggingMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		raw := c.Request.URL.RawQuery

		c.Next()

		if raw != "" {
			path = path + "?" + raw
		}

		log.Info().
			Str("method", c.Request.Method).
			Str("path", path).
			Int("status", c.Writer.Status()).
			Dur("duration", time.Since(start)).
			Str("client_ip", c.ClientIP()).
			Msg("Request processed")
	}
}

func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusOK)
			return
		}

		c.Next()
	}
}
