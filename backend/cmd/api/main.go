package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"nyx/internal/middleware"
	"nyx/internal/movie"
	"nyx/internal/platform/database"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func main() {
	// Configure zerolog
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = log.Output(os.Stdout)

	dbURL := os.Getenv("DB_URL")
	if dbURL == "" {
		log.Fatal().Msg("DB_URL environment variable is required")
	}

	// Initialize database
	db, err := database.New(dbURL)
	if err != nil {
		log.Fatal().Err(err).Msg("Could not connect to database after retries")
	}
	defer db.Close()

	// Run migrations
	database.RunMigrations(dbURL)

	// Initialize layers
	movieRepo := movie.NewRepository(db)
	movieService := movie.NewService(movieRepo)
	movieHandler := movie.NewHandler(movieService)

	// Set up Gin
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()

	// Middleware
	router.Use(gin.Recovery())
	router.Use(middleware.Logging())
	router.Use(middleware.CORS())

	// Routes
	api := router.Group("/api")
	{
		api.GET("/health", movieHandler.HealthHandler)
		api.GET("/movies", movieHandler.GetMoviesHandler)
		api.POST("/movies", movieHandler.CreateMovieHandler)
		api.PUT("/movies/:id", movieHandler.UpdateMovieHandler)
		api.DELETE("/movies/:id", movieHandler.DeleteMovieHandler)
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
