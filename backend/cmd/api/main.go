package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"nyx/internal/chat"
	"nyx/internal/feed"
	"nyx/internal/llm"
	"nyx/internal/llm/openai"
	"nyx/internal/llm/vllm"
	"nyx/internal/mcp"
	"nyx/internal/middleware"
	"nyx/internal/platform/api"
	"nyx/internal/platform/auth"
	"nyx/internal/platform/cache"
	"nyx/internal/platform/config"
	"nyx/internal/platform/database"
	"nyx/internal/platform/observability"
	"nyx/internal/rag"
	"nyx/internal/user"

	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

//nolint:gocyclo // Wiring complexity: each block is a distinct lifecycle stage (config → token → tracing → DB → cache → feed → user → chat) extracted for readability. Splitting further would scatter the boot sequence across multiple files without simplifying the logic.
func main() {
	// Configure zerolog
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = log.Output(os.Stdout)

	cfg := loadConfigOrFatal()
	buildAndServe(cfg)
}

// loadConfigOrFatal loads config and runs the post-load invariants
// that must hold before any domain construction (DB_URL non-empty
// is the only one). config.Load already validates JWT_SECRET +
// DB_URL via its cross-field checks; this is the belt-and-braces
// guard for the rare case where config.Load succeeds but the
// operator forgot to set DB_URL.
func loadConfigOrFatal() *config.Config {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal().Err(err).Msg("Could not load configuration")
	}
	if cfg.DBURL == "" {
		log.Fatal().Msg("DB_URL environment variable is required")
	}
	return cfg
}

// buildAndServe assembles every domain (config, token, tracing,
// DB, cache, feed, user, chat) and starts the HTTP server. The
// graceful-shutdown wiring sits in the final defer block. Errors
// during startup fail closed via log.Fatal — the operator sees
// a clear, single-line root cause in the boot log.
func buildAndServe(cfg *config.Config) {

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

	// Run migrations BEFORE the pool opens. golang-migrate opens its
	// own short-lived connection, so the pool's AfterConnect (which
	// registers the pgvector codec via pgxvec.RegisterTypes) doesn't
	// have to be tolerant of "type not found" — by the time the pool
	// spins up its first connection, migration 000014 has already run
	// CREATE EXTENSION vector. Running migrations first also means a
	// fresh dev volume gets its schema on boot without requiring a
	// separate `make up && go run cmd/api` dance.
	//
	// golang-migrate's API is synchronous and does not accept a
	// context — RunMigrations therefore takes no ctx. Failures are
	// fatal at the caller; RunMigrations only returns errors.
	if migErr := database.RunMigrations(cfg.DBURL, cfg.MigrationPath); migErr != nil {
		log.Fatal().Err(migErr).Msg("migrations failed")
	}

	// Initialize database. The pool's AfterConnect hook registers
	// pgvector's vector / halfvec / sparsevec codecs; this fires
	// per connection. Migration 000014 installed the extension in
	// the step above, so the registration query finds the type.
	db, err := database.New(context.Background(), cfg, observability.NewPgxTracer(tracing.Provider))
	if err != nil {
		log.Fatal().Err(err).Msg("Could not connect to database")
	}
	defer db.Close()

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

	// Initialize Feed domain. The indexer starts as noop; cmd/api/main.go
	// swaps it for the real rag.Indexer below once the chat/embed
	// wiring is built (LLM is a prerequisite for embeddings).
	var feedIndexer feed.EmbeddingIndexer = feed.NoopEmbeddingIndexer{}
	feedRepo := feed.NewRepository(db)
	feedService := feed.NewService(feedRepo, cacheClient, feedIndexer, cacheTTL, tracing.Provider.Tracer("nyx.feed"))
	feedHandler := feed.NewHandler(feedService)

	// Initialize User domain. accessTTL / refreshTTL come from viper
	// (JWT_ACCESS_TTL / JWT_REFRESH_TTL); config.Load has already
	// validated them as positive durations and refresh > access.
	userRepo := user.NewRepository(db)
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

	feed.RegisterFeedOps(humaAPI, feedHandler, tokens)
	user.RegisterUserOps(humaAPI, userHandler, tokens)

	// Build the LLM provider (single-provider or router with
	// failover) + the RAG facade (nil when LLM is disabled). The
	// ragSvc drives the chat service's RAG injection; the indexer
	// swaps the feed service's noop indexer so subsequent writes
	// embed.
	stopChatGC, _, ragIndexer := mountChatRoute(router, cfg, db, feedRepo, tracing.Provider, tokens)
	defer stopChatGC()
	if ragIndexer != nil {
		feedService.SetIndexer(ragIndexer)
	}

	// MCP server — gated on MCP_ENABLED. Default-off so deployments
	// without external agents pay no allocation. See FUTURE_BACKEND.md
	// §11 for the architecture and per-owner auth contract.
	stopMCPLimiter := mountMCPRoute(router, cfg, feedService, tracing.Provider, tokens)
	defer stopMCPLimiter()

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

// mountChatRoute wires the optional /api/chat SSE endpoint when
// LLM_ENABLED=true. When LLM_ENABLED=false no route is mounted and
// no provider client is constructed, so deployments without an LLM
// pay nothing. The same router is reused (chat.RegisterChatRoute
// mounts directly on chi, bypassing huma — see backend/HUMA.md
// "Streaming endpoints" for the rationale).
//
// LLM_PROVIDER picks the primary client implementation: "openai"
// (default; targets OpenAI proper or any OpenAI-compatible server
// via the sashabaranov SDK) or "vllm" (raw HTTP + hand-rolled SSE
// decoder for self-hosted vLLM, with per-dial DNS hardening). The
// fallback (LLM_FALLBACK_BASE_URL) is the opposite provider type;
// both slots are validated at config.Load() and either may be nil
// (single-provider passthrough — zero per-request probe cost).
//
// The chat domain consumes the resulting client through the
// llm.Provider interface and never imports either implementation
// directly. provider / model / base_url are logged at Info on
// startup so the operator can confirm their config took; the API
// key is NEVER logged. Returns a shutdown function the caller
// MUST defer so the chat-rate-limiter's GC goroutine stops on
// graceful shutdown.
//
// feedRepo is the feed-domain persistence handle — the RAG facade
// uses it for the lazy backfill's "feeds without embeddings"
// query. The second + third return values are the constructed
// RAG facade and its indexer (both nil when LLM is disabled) so
// main() can swap the feed service's noop indexer for the real
// rag.Indexer.
func mountChatRoute(router chi.Router, cfg *config.Config, db *pgxpool.Pool, feedRepo feed.Repository, tracerProvider trace.TracerProvider, tokens auth.TokenService) (shutdown func(), ragSvc *rag.Service, ragIndexer feed.EmbeddingIndexer) {
	if !cfg.LLMEnabled {
		log.Info().Msg("LLM chat disabled (LLM_ENABLED=false)")
		return func() {}, nil, nil
	}

	// Build the primary client from the active config.
	primaryClient, err := buildLLMClient(cfg, tracerProvider)
	if err != nil {
		log.Fatal().Err(err).Str("provider", cfg.LLMProvider).Msg("primary LLM client init failed")
	}
	primary := &llm.ProviderSlot{
		Name:     cfg.LLMProvider,
		Provider: primaryClient,
		BaseURL:  cfg.LLMBaseURL,
		ProbeKey: probeKeyFor(cfg, cfg.LLMProvider),
	}

	// Build the fallback client from a *copy* of cfg with the three
	// fallback-specific fields overridden. Clone-not-mutate keeps
	// the caller's Config unchanged for any downstream consumer.
	var secondary *llm.ProviderSlot
	if cfg.LLMFallbackBaseURL != "" {
		fallbackProvider := config.LLMProviderOpenAI
		if cfg.LLMProvider == config.LLMProviderOpenAI {
			fallbackProvider = config.LLMProviderVLLM
		}
		cfg2 := *cfg
		cfg2.LLMProvider = fallbackProvider
		cfg2.LLMBaseURL = cfg.LLMFallbackBaseURL
		cfg2.LLMAPIKey = cfg.LLMFallbackAPIKey
		if cfg.LLMFallbackModel != "" {
			cfg2.LLMModel = cfg.LLMFallbackModel
		}
		fallbackClient, ferr := buildLLMClient(&cfg2, tracerProvider)
		if ferr != nil {
			log.Fatal().Err(ferr).Str("provider", fallbackProvider).Msg("fallback LLM client init failed")
		}
		secondary = &llm.ProviderSlot{
			Name:     fallbackProvider,
			Provider: fallbackClient,
			BaseURL:  cfg.LLMFallbackBaseURL,
			ProbeKey: probeKeyFor(&cfg2, fallbackProvider),
		}
	}

	// Pick the Provider the chat service consumes. Dual-provider
	// → Router with per-request probe; single-provider →
	// passthrough, no probe cost.
	var llmClient llm.Provider
	switch {
	case secondary != nil:
		probeTimeout, perr := time.ParseDuration(cfg.LLMProbeTimeout)
		if perr != nil {
			log.Fatal().Err(perr).Msg("invalid LLM_PROBE_TIMEOUT")
		}
		llmClient = llm.NewRouter(
			*primary,
			secondary,
			tracerProvider.Tracer("nyx.llm.router"),
			probeTimeout,
		)
		log.Info().
			Str("primary", primary.Name).
			Str("secondary", secondary.Name).
			Str("probe_timeout", cfg.LLMProbeTimeout).
			Msg("LLM router: failover enabled")
	default:
		llmClient = primary.Provider
		log.Info().
			Str("provider", primary.Name).
			Str("model", cfg.LLMModel).
			Str("base_url", cfg.LLMBaseURL).
			Msg("LLM chat enabled")
	}

	// RAG facade. Built only when LLM is enabled (embeddings need
	// an LLM). When LLM_EMBEDDING_BASE_URL is set we build a
	// dedicated llm.Provider for /v1/embeddings (a separate model
	// — usually an embedding-tuned model — at a separate endpoint).
	// Otherwise the embedder reuses the chat provider (Router or
	// single) and falls back to LLM_MODEL for the embed model name
	// when LLM_EMBEDDING_MODEL is empty. See mountRAGFacade for the
	// extraction.
	if cfg.LLMEnabled {
		ragSvc, ragIndexer = mountRAGFacade(cfg, db, feedRepo, primary, llmClient, tracerProvider)
	}

	chatService := chat.NewService(llmClient, cfg, ragSvc, tracerProvider.Tracer("nyx.chat"))
	chatHandler := chat.NewHandler(chatService)
	// Per-user limiter: 5 streams/min, burst 3. In-memory only;
	// see chat/ratelimit.go for the cost trade-off.
	chatLimiter := chat.NewUserRateLimiter(5.0/60.0, 3)
	stopGC := chatLimiter.RunGC(time.Minute, time.Hour)
	chat.RegisterChatRoute(router, chatHandler, tokens, chatLimiter)
	return stopGC, ragSvc, ragIndexer
}

// mountRAGFacade builds the RAG stack (embedder / indexer /
// retriever / service) and wires the right llm.Provider to the
// embedder.
//
// When LLM_EMBEDDING_BASE_URL is set, the embedder gets a
// dedicated client (built from a clone of cfg with the embed
// fields swapped in, mirroring the fallback-slot pattern above).
// The dedicated client is single-provider — the embed endpoint
// has no LLM_FALLBACK_* counterpart, and a failed embed is
// best-effort by contract (feed/service.go:151 logs Warn and
// continues on failure).
//
// When LLM_EMBEDDING_BASE_URL is empty, the embedder reuses the
// chat provider (Router or single) and falls back to LLM_MODEL
// for the embed model name when LLM_EMBEDDING_MODEL is empty —
// today's behaviour for single-model deployments.
//
// Returns the RAG service (nil when not built) and the indexer
// (nil when not built) so the caller can swap the feed service's
// noop indexer for the real rag.Indexer.
func mountRAGFacade(cfg *config.Config, db *pgxpool.Pool, feedRepo feed.Repository, primary *llm.ProviderSlot, chatClient llm.Provider, tracerProvider trace.TracerProvider) (*rag.Service, feed.EmbeddingIndexer) {
	var ragLLMClient llm.Provider = chatClient
	var embeddingModel string
	if cfg.LLMEmbeddingBaseURL != "" {
		embedProvider := cfg.LLMEmbeddingProvider
		if embedProvider == "" {
			embedProvider = cfg.LLMProvider
		}
		embedAPIKey := cfg.LLMEmbeddingAPIKey
		if embedAPIKey == "" {
			embedAPIKey = cfg.LLMAPIKey
		}
		cfgEmbed := *cfg
		cfgEmbed.LLMProvider = embedProvider
		cfgEmbed.LLMBaseURL = cfg.LLMEmbeddingBaseURL
		cfgEmbed.LLMAPIKey = embedAPIKey
		cfgEmbed.LLMModel = cfg.LLMEmbeddingModel
		cfgEmbed.LLMAllowPrivateURL = cfg.LLMEmbeddingAllowPrivateURL || cfg.LLMAllowPrivateURL
		embedClient, eerr := buildLLMClient(&cfgEmbed, tracerProvider)
		if eerr != nil {
			log.Fatal().Err(eerr).Str("provider", embedProvider).Msg("embedding LLM client init failed")
		}
		ragLLMClient = embedClient
		embeddingModel = cfg.LLMEmbeddingModel
		log.Info().
			Str("embed_provider", embedProvider).
			Str("embed_base_url", cfg.LLMEmbeddingBaseURL).
			Str("embed_model", cfg.LLMEmbeddingModel).
			Msg("RAG using separate embed provider")
	} else {
		embeddingModel = cfg.LLMEmbeddingModel
		if embeddingModel == "" {
			embeddingModel = cfg.LLMModel
		}
		log.Info().
			Str("embed_provider", primary.Name).
			Str("embed_model", embeddingModel).
			Msg("RAG reusing chat LLM provider for embeddings")
	}
	ragEmbedder := rag.NewEmbedder(ragLLMClient, embeddingModel, tracerProvider.Tracer("nyx.rag"))
	ragIndexer := rag.NewIndexer(db, ragEmbedder, tracerProvider.Tracer("nyx.rag"))
	ragRetriever := rag.NewRetriever(db, tracerProvider.Tracer("nyx.rag"))
	ragSvc := rag.NewService(
		ragEmbedder, ragRetriever, db, feedRepo,
		cfg.RAGTopK, cfg.RAGMaxContextChars, cfg.RAGMaxBackfillPerRequest,
		tracerProvider.Tracer("nyx.rag"),
	)
	// Schema-drift warning: warn loudly if the live
	// feed_embeddings.embedding column dim disagrees with
	// EMBEDDING_DIMENSIONS. Doesn't fail-closed (the operator may
	// be intentionally migrating), but logs the remediation
	// runbook so a wrong-config deployment is obvious in the boot
	// log.
	warnEmbeddingDimMismatch(db, cfg.EmbeddingDimensions)
	log.Info().
		Str("embedding_model", embeddingModel).
		Int("dimensions", cfg.EmbeddingDimensions).
		Int("top_k", cfg.RAGTopK).
		Int("max_context_chars", cfg.RAGMaxContextChars).
		Msg("RAG over feeds enabled")
	return ragSvc, ragIndexer
}

// buildLLMClient dispatches on cfg.LLMProvider to the right client
// constructor. The choice is validated by config.Load; an unknown
// value here would already have failed boot, so we log + return an
// error rather than falling back silently.
func buildLLMClient(cfg *config.Config, tracerProvider trace.TracerProvider) (llm.Provider, error) {
	switch cfg.LLMProvider {
	case config.LLMProviderVLLM:
		return vllm.NewClient(cfg, tracerProvider.Tracer("nyx.llm.vllm"))
	case config.LLMProviderOpenAI, "":
		return openai.NewClient(cfg, tracerProvider.Tracer("nyx.llm.openai"))
	default:
		return nil, fmt.Errorf("unknown LLM_PROVIDER %q", cfg.LLMProvider)
	}
}

// mountMCPRoute installs the chi-direct /mcp route when
// MCP_ENABLED=true. Mirror of mountChatRoute: gated on the feature
// flag, constructs service + handler + per-user limiter, returns
// the GC stop function the caller must call on graceful shutdown.
//
// The MCP server itself wraps feed.Service — it never imports a
// repository directly. Per-owner scoping is enforced inside
// feed.Service via the auth-stamped context that chi's NewAuth
// middleware writes before the route handler runs. Cross-owner
// reads bubble up as feed.ErrNotFound (single sentinel, no
// existence leak) — same contract as the REST surface.
func mountMCPRoute(router chi.Router, cfg *config.Config, feedSvc feed.Service, tracerProvider trace.TracerProvider, tokens auth.TokenService) func() {
	if !cfg.MCPEnabled {
		log.Info().Msg("MCP server disabled (MCP_ENABLED=false)")
		return func() {}
	}

	mcpSvc := mcp.NewService(feedSvc, tracerProvider.Tracer("nyx.mcp"))
	mcpHandler := mcp.NewHandler(mcpSvc, cfg.MCPPath, cfg.MCPMaxBodyBytes)

	// Per-user limiter: 1 token/sec sustained (60 calls/min) with
	// burst 20. In-memory only; see internal/mcp/ratelimit.go for
	// the cost trade-off. The chat limiter is 5 streams/min because
	// each stream holds open for minutes; MCP tool calls are cheap
	// short-lived RPCs so the cap is ten times higher.
	mcpLimiter := mcp.NewUserRateLimiter(1.0, 20)
	stopGC := mcpLimiter.RunGC(time.Minute, time.Hour)
	mcp.RegisterMCPRoute(router, mcpHandler, tokens, mcpLimiter)

	log.Info().
		Str("path", cfg.MCPPath).
		Int64("max_body_bytes", cfg.MCPMaxBodyBytes).
		Msg("MCP server enabled")

	return stopGC
}

// probeKeyFor returns the bearer key the per-request GET /v1/models
// probe should send as Authorization. vLLM allows an empty key
// (started without --api-key); OpenAI requires a non-empty one.
// cfg.LLMProvider drives the policy; the config validator already
// enforced it, so this is the straight read.
func probeKeyFor(cfg *config.Config, provider string) string {
	return cfg.LLMAPIKey
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

// warnEmbeddingDimMismatch queries pg_attribute for the
// feed_embeddings.embedding column's stored dim and warns loudly
// when it disagrees with cfg.EmbeddingDimensions. pgvector stores
// the dim in atttypmod (so vector(1536) → 1536 directly).
//
// Doesn't fail-closed: an operator may be intentionally migrating
// (truncating the table, swapping models, about to run a new
// migration). The warning is loud enough to make a wrong-config
// deployment obvious in the boot log. A migration to drop+
// recreate the column would emit the same warning on the
// intermediate boot, which is the correct behaviour.
func warnEmbeddingDimMismatch(db *pgxpool.Pool, expected int) {
	if db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var dim int
	err := db.QueryRow(ctx,
		`SELECT atttypmod FROM pg_attribute
		 WHERE attrelid = 'feed_embeddings'::regclass
		   AND attname   = 'embedding'`).Scan(&dim)
	if err != nil {
		// Table doesn't exist yet (testcontainers racing migrations)
		// or pgvector isn't installed. Either way, defer to the
		// migration step which surfaces its own clear errors.
		return
	}
	if dim != expected {
		log.Warn().
			Int("schema_dim", dim).
			Int("configured_dim", expected).
			Msg("feed_embeddings.embedding dim mismatch — run a migration to drop+recreate the column (see FUTURE_BACKEND.md §rag), or set EMBEDDING_DIMENSIONS to match the live column")
	}

}
