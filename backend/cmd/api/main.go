package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"nyx/internal/chat"
	"nyx/internal/llm/openai"
	"nyx/internal/middleware"
	"nyx/internal/movie"
	"nyx/internal/platform/api"
	"nyx/internal/platform/auth"
	"nyx/internal/platform/cache"
	"nyx/internal/platform/config"
	"nyx/internal/platform/database"
	"nyx/internal/platform/observability"
	"nyx/internal/user"
	userdb "nyx/internal/user/db"

	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
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
	// in prod. accessTTL is captured here so the configured TTL
	// (JWT_ACCESS_TTL, default 15m) is the only lifetime the service
	// ever mints with.
	accessTTL, err := time.ParseDuration(cfg.JWTAccessTTL)
	if err != nil {
		log.Fatal().Err(err).Msg("invalid JWT_ACCESS_TTL")
	}
	refreshTTL, err := time.ParseDuration(cfg.JWTRefreshTTL)
	if err != nil {
		log.Fatal().Err(err).Msg("invalid JWT_REFRESH_TTL")
	}
	tokens, err := auth.NewTokenService([]byte(cfg.JWTSecret), accessTTL)
	if err != nil {
		log.Fatal().Err(err).Msg("invalid JWT secret")
	}

	// Initialise OpenTelemetry tracing. When OTEL_ENABLED is false
	// (the default) this returns a noop provider + no-op shutdown —
	// every tracer.Start becomes free and pgx skips its tracer
	// callback entirely. When enabled, the OTLP/HTTP exporter
	// buffers spans and flushes every 5s; the deferred Shutdown
	// flushes any remainder on SIGTERM (3s deadline, separate ctx
	// so server.Shutdown's deadline doesn't truncate it).
	tracing := setupTracing(cfg)
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if shutdownErr := tracing.Shutdown(sctx); shutdownErr != nil {
			log.Warn().Err(shutdownErr).Msg("tracer shutdown error")
		}
	}()

	// Initialize database
	db, err := database.New(context.Background(), cfg, observability.NewPgxTracer(tracing.Provider))
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
		rc, redisErr := connectRedisWithRetry(cfg.RedisURL, 10)
		if redisErr != nil {
			log.Fatal().Err(redisErr).Msg("Could not connect to Redis")
		}
		cacheClient = rc
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
	movieService := movie.NewService(movieRepo, cacheClient, cacheTTL, tracing.Provider.Tracer("nyx.movie"))
	movieHandler := movie.NewHandler(movieService)

	// Initialize User domain. accessTTL / refreshTTL come from viper
	// (JWT_ACCESS_TTL / JWT_REFRESH_TTL); config.Load has already
	// validated them as positive durations and refresh > access.
	userRepo := user.NewRepository(userdb.New(db))
	userService := user.NewService(userRepo, tokens, accessTTL, refreshTTL, tracing.Provider.Tracer("nyx.user"))

	// Background refresh-token cleanup. The goroutine sweeps for
	// revoked/expired rows older than RefreshTokenRetention and
	// deletes them in bulk. Stops on graceful shutdown so the
	// process doesn't leak the goroutine past SIGTERM (see
	// SECURITY.md M2).
	stopCleanup := user.StartRefreshCleanup(userRepo)
	defer stopCleanup()

	// TokenService is the single source of truth for auth-token
	// validation; both the auth middleware (which reads the
	// Authorization header) and the user handler (which mints /
	// rotates refresh tokens) consume the same struct.
	userHandler := user.NewHandler(userService)

	// Build chi router. Middleware order (outermost first):
	//   Tracing → RequestID → StoreRequest → RealIP → Recoverer →
	//     Prometheus → Logging → CORS → RateLimit → maxBodyBytes
	// Tracing sits first so the OTel server span is the parent of
	// every child span the application opens (service, pgx). StoreRequest
	// sits next so handlers and middlewares can recover the live
	// *http.Request via reqctx.RequestFromContext (huma's generic
	// handler signature is func(context.Context, *I) — no direct
	// request access). CORS sits inside logging so OPTIONS preflight
	// failures still get logged; rate-limit sits inside CORS so a
	// throttled request still returns CORS headers.
	router := chi.NewRouter()
	router.Use(middleware.Tracing(cfg.OTelServiceName))
	router.Use(middleware.RequestID)
	router.Use(middleware.StoreRequest)
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
	user.RegisterUserOps(humaAPI, userHandler, tokens)

	// Chat domain. Opt-in via LLM_ENABLED; when off, no route is
	// mounted and no provider client is constructed. The same
	// router is reused (chat.RegisterChatRoute mounts directly on
	// chi, bypassing huma — see backend/HUMA.md "Streaming
	// endpoints" for the rationale). model + base_url are logged
	// at Info on startup so the operator can confirm the operator's
	// config took; the API key is NEVER logged.
	if cfg.LLMEnabled {
		llmClient, llmErr := openai.NewClient(cfg, tracing.Provider.Tracer("nyx.llm.openai"))
		if llmErr != nil {
			log.Fatal().Err(llmErr).Msg("LLM client init failed")
		}
		chatService := chat.NewService(llmClient, cfg, tracing.Provider.Tracer("nyx.chat"))
		chatHandler := chat.NewHandler(chatService)
		// Per-user limiter: 5 streams/min, burst 3. In-memory only;
		// see chat/ratelimit.go for the cost trade-off.
		chatLimiter := chat.NewUserRateLimiter(5.0/60.0, 3)
		stopChatGC := chatLimiter.RunGC(time.Minute, time.Hour)
		defer stopChatGC()
		chat.RegisterChatRoute(router, chatHandler, tokens, chatLimiter)
		log.Info().
			Str("model", cfg.LLMModel).
			Str("base_url", cfg.LLMBaseURL).
			Msg("LLM chat enabled")
	} else {
		log.Info().Msg("LLM chat disabled (LLM_ENABLED=false)")
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

// maxBodyBytesLimit caps request bodies at 1 MiB. huma parses the body
// BEFORE per-operation Middlewares run, so a 100 MB POST could trigger
// work before being rejected by Auth. http.MaxBytesReader enforces the
// cap at the io.Reader level — huma's JSON decoder sees io.ErrUnexpectedEOF
// when the limit is hit and surfaces a 400.
const maxBodyBytesLimit = 1 << 20

// setupTracing initialises the OTel tracer provider and installs the
// W3C TraceContext + Baggage composite propagator on the global. On
// OTEL_ENABLED=false this is a noop setup; on enabled it returns a
// live Tracing whose Shutdown must be deferred by the caller.
//
// Extracted from main() to keep the wiring function under the
// project's cyclomatic-complexity budget.
func setupTracing(cfg *config.Config) *observability.Tracing {
	tracing, err := observability.Setup(context.Background(), cfg)
	if err != nil {
		log.Fatal().Err(err).Msg("tracing setup failed")
	}
	otel.SetTracerProvider(tracing.Provider)
	// Composite propagator: W3C TraceContext + Baggage. Without
	// these, inbound `traceparent` headers from upstream calls are
	// silently dropped and we never join the upstream trace.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return tracing
}

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
