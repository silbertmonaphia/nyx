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
	"nyx/internal/platform/api"
	"nyx/internal/platform/auth"
	"nyx/internal/platform/cache"
	"nyx/internal/platform/config"
	"nyx/internal/platform/database"
	"nyx/internal/user"
	userdb "nyx/internal/user/db"

	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

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

	// Build the JWT TokenService. config.Load already validated that
	// cfg.JWTSecret is non-default and at least MinSecretBytes long;
	// NewTokenService repeats the check so a future config drift can't
	// sign tokens with a weak key. Fails closed at startup, not silently
	// in prod.
	tokens, err := auth.NewTokenService([]byte(cfg.JWTSecret))
	if err != nil {
		log.Fatal().Err(err).Msg("invalid JWT secret")
	}

	// Initialize database
	db, err := database.New(cfg)
	if err != nil {
		log.Fatal().Err(err).Msg("Could not connect to database")
	}
	defer db.Close()

	// Run migrations. golang-migrate's API is synchronous and does not
	// accept a context — RunMigrations therefore takes no ctx. Failures
	// are fatal at the caller; RunMigrations only returns errors.
	if migErr := database.RunMigrations(cfg.DBURL, cfg.MigrationPath); migErr != nil {
		log.Fatal().Err(migErr).Msg("migrations failed")
	}

	// Initialize cache. Disabled by default; when enabled we wait up to
	// 10s for Redis to come up so we fail fast on misconfiguration rather
	// than serving cache errors on every request.
	var cacheClient cache.Cache = cache.NewNoop()
	if cfg.RedisEnabled {
		var err error
		cacheClient, err = connectRedisWithRetry(cfg.RedisURL, 10)
		if err != nil {
			log.Fatal().Err(err).Msg("Could not connect to Redis")
		}
		defer func() { _ = cacheClient.Close() }()
		log.Info().Str("url", cfg.RedisURL).Msg("Cache enabled")
	} else {
		log.Info().Msg("Cache disabled (REDIS_ENABLED=false); using no-op cache")
	}

	cacheTTL, err := time.ParseDuration(cfg.CacheTTL)
	if err != nil {
		log.Fatal().Err(err).Msg("Invalid CACHE_TTL")
	}

	// Initialize Movie domain
	movieRepo := movie.NewRepository(db)
	movieService := movie.NewService(movieRepo, cacheClient, cacheTTL)
	movieHandler := movie.NewHandler(movieService)

	// Initialize User domain
	userRepo := user.NewRepository(userdb.New(db))
	userService := user.NewService(userRepo, tokens)
	userHandler := user.NewHandler(userService)

	// Build chi router. Middleware order (outermost first):
	//   RequestID → Recoverer → Prometheus → Logging → CORS → RateLimit
	// CORS sits inside logging so OPTIONS preflight failures still get
	// logged; rate-limit sits inside CORS so a throttled request still
	// returns CORS headers.
	router := chi.NewRouter()
	router.Use(middleware.RequestID)
	router.Use(middleware.RealIP)
	router.Use(middleware.Recoverer)
	router.Use(middleware.Prometheus)
	router.Use(middleware.Logging)
	router.Use(middleware.NewCORS(middleware.SplitNonEmpty(cfg.CORSAllowedOrigins)))
	router.Use(middleware.DefaultRateLimit())
	router.Use(maxBodyBytes(maxBodyBytesLimit))

	// Mount /metrics at the router root, BEFORE huma wraps it. Prometheus
	// scrapers do not speak our error envelope and don't carry a JWT, so
	// they must skip huma's request processing entirely.
	router.Handle("/metrics", promhttp.Handler())

	// Override huma's package-level error constructors so every error
	// (validation failures, panic recovery, the prebuilt 4xx/5xx helpers,
	// handler-returned errors) flows through the legacy {error, code,
	// request_id, details} envelope the frontend already parses.
	api.OverrideHumaErrors()

	// Build the huma API on top of chi. humachi adapts chi's URL params
	// into huma.Operation.Path params and lets huma use chi's router for
	// dispatch. Order matters: routes are registered against the chi
	// router and the huma API is built on top of it via NewAdapter.
	// api.HumaConfig() is shared with cmd/openapi so the committed spec
	// artifact cannot drift from what the server serves.
	humaAPI := humachi.New(router, api.HumaConfig())

	movie.RegisterMovieOps(humaAPI, movieHandler, tokens)
	user.RegisterUserOps(humaAPI, userHandler)

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

// maxBodyBytesLimit caps request bodies at 1 MiB. huma parses the body
// BEFORE per-operation Middlewares run, so a 100 MB POST could trigger
// work before being rejected by Auth. http.MaxBytesReader enforces the
// cap at the io.Reader level — huma's JSON decoder sees io.ErrUnexpectedEOF
// when the limit is hit and surfaces a 400.
const maxBodyBytesLimit = 1 << 20

// maxBodyBytes returns a middleware that wraps r.Body in an
// http.MaxBytesReader so oversized payloads fail fast.
func maxBodyBytes(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil && r.ContentLength != 0 {
				r.Body = http.MaxBytesReader(w, r.Body, limit)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// connectRedisWithRetry mirrors database.New's startup retry pattern so
// the API tolerates a Redis container that is still booting.
func connectRedisWithRetry(url string, attempts int) (cache.Cache, error) {
	var lastErr error
	for i := 0; i < attempts; i++ {
		c, err := cache.NewRedis(url)
		if err == nil {
			return c, nil
		}
		lastErr = err
		time.Sleep(1 * time.Second)
	}
	return nil, lastErr
}
