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
	"nyx/internal/platform/config"
	"nyx/internal/platform/database"
	"nyx/internal/user"

	_ "nyx/docs"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
	"github.com/zsais/go-gin-prometheus"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// @title Nyx API
// @version 1.0
// @description Minimalist media rating application API.
// @host localhost:8080
// @BasePath /api

// @securityDefinitions.apikey ApiKeyAuth
// @in header
// @name Authorization

func main() {
	// Configure zerolog
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = log.Output(os.Stdout)

	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		log.Fatal().Err(err).Msg("Could not load configuration")
	}

	if cfg.DBURL == "" {
		log.Fatal().Msg("DB_URL environment variable is required")
	}

	// Initialize database
	db, err := database.New(cfg.DBURL)
	if err != nil {
		log.Fatal().Err(err).Msg("Could not connect to database")
	}
	defer db.Close()

	// Run migrations
	database.RunMigrations(cfg.DBURL)

	// Initialize Movie domain
	movieRepo := movie.NewRepository(db)
	movieService := movie.NewService(movieRepo)
	movieHandler := movie.NewHandler(movieService)

	// Initialize User domain
	userRepo := user.NewRepository(db)
	userService := user.NewService(userRepo)
	userHandler := user.NewHandler(userService)

	// Set up Gin
	gin.SetMode(cfg.GinMode)
	router := gin.New()

	// Metrics
	p := ginprometheus.NewPrometheus("gin")
	p.Use(router)

	// Middleware
	router.Use(gin.Recovery())
	router.Use(middleware.RequestID())
	router.Use(middleware.Logging())
	router.Use(middleware.CORS())
	router.Use(middleware.DefaultRateLimit())

	// API Routes
	api := router.Group("/api")
	{
		api.GET("/health", movieHandler.HealthHandler)
		url := ginSwagger.URL("/api/swagger/doc.json")
		api.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler, url))

		// Auth routes
		api.POST("/register", userHandler.Register)
		api.POST("/login", userHandler.Login)

		// Movie routes
		movies := api.Group("/movies")
		{
			movies.GET("", movieHandler.GetMoviesHandler)
			
			// Protected routes
			protected := movies.Group("")
			protected.Use(middleware.Auth())
			{
				protected.POST("", movieHandler.CreateMovieHandler)
				protected.PUT("/:id", movieHandler.UpdateMovieHandler)
				protected.DELETE("/:id", movieHandler.DeleteMovieHandler)
			}
		}
	}

	port := ":" + cfg.Port
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
