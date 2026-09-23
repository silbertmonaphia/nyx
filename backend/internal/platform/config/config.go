package config

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/viper"
	"nyx/internal/platform/auth"
)

// LLM provider identifiers. Centralised as constants so the
// validateLLMConfig switch, the cmd/api/main.go dispatch, and
// the config_test.go golden-value assertions all reference the
// same literals (goconst would otherwise flag the duplicates).
// Exported so cmd/api/main.go can reference them without
// re-declaring the same strings.
const (
	LLMProviderOpenAI = "openai"
	LLMProviderVLLM   = "vllm"
)

type Config struct {
	DBURL         string `mapstructure:"DB_URL"`
	JWTSecret     string `mapstructure:"JWT_SECRET"`
	JWTAccessTTL  string `mapstructure:"JWT_ACCESS_TTL"`
	JWTRefreshTTL string `mapstructure:"JWT_REFRESH_TTL"`
	Port          string `mapstructure:"PORT"`
	MigrationPath string `mapstructure:"MIGRATION_PATH"`

	// Database Connection Pool
	DBMaxOpenConns    int    `mapstructure:"DB_MAX_OPEN_CONNS"`
	DBMaxIdleConns    int    `mapstructure:"DB_MAX_IDLE_CONNS"`
	DBConnMaxLifetime string `mapstructure:"DB_CONN_MAX_LIFETIME"`
	DBConnMaxIdleTime string `mapstructure:"DB_CONN_MAX_IDLE_TIME"`

	// Cache
	RedisURL     string `mapstructure:"REDIS_URL"`
	RedisEnabled bool   `mapstructure:"REDIS_ENABLED"`
	CacheTTL     string `mapstructure:"CACHE_TTL"`

	// CORS — comma-separated allowlist. Default is empty (deny-all
	// cross-origin) per the project's deny-by-default safety
	// convention. Operators MUST set this to the explicit list of
	// frontend origins (e.g. "https://app.example.com,https://staging.example.com").
	//
	// "*" is supported as an explicit opt-in for fully public APIs
	// that carry no credentials. Note that "*" + "Authorization" in
	// Access-Control-Allow-Headers is invalid per the CORS spec
	// (browsers refuse the credentialed header under "*"), and
	// combining "*" with cookie-based auth is similarly unsafe; if
	// you turn this on, audit the auth surface.
	CORSAllowedOrigins string `mapstructure:"CORS_ALLOWED_ORIGINS"`

	// OpenTelemetry — distributed tracing.
	//
	// OTEL_ENABLED is the master switch. When false (default) the SDK
	// is replaced with a noop provider so every tracer.Start call is a
	// free no-op and there is zero export overhead. Production /
	// staging flips this on.
	//
	// OTEL_EXPORTER_OTLP_ENDPOINT is a full URL (e.g.
	// "http://localhost:4318" or "https://collector.example.com:4318").
	// Whether the client uses plaintext or TLS is derived from the URL
	// scheme — passing "http://" forces plaintext, "https://" forces
	// TLS. Mixing in a separate insecure flag would let a misconfigured
	// env pair downgrade an https URL to plaintext (the last option
	// applied wins), so we don't expose that knob.
	//
	// OTEL_TRACES_SAMPLER / OTEL_TRACES_SAMPLER_ARG are read directly
	// by the OTel SDK (see https://opentelemetry.io/docs/specs/otel/
	// configuration/sdk-environment-variables/) and intentionally not
	// surfaced here.
	OTelEnabled              bool   `mapstructure:"OTEL_ENABLED"`
	OTelServiceName          string `mapstructure:"OTEL_SERVICE_NAME"`
	OTelExporterOTLPEndpoint string `mapstructure:"OTEL_EXPORTER_OTLP_ENDPOINT"`

	// LLM (chat). Off by default; when enabled the operator MUST set
	// LLM_BASE_URL and LLM_MODEL. LLM_PROVIDER selects the client
	// implementation: "openai" (default, works against any
	// OpenAI-compatible server via sashabaranov/go-openai) or
	// "vllm" (raw HTTP + hand-rolled SSE decoder for self-hosted
	// vLLM). LLM_BASE_URL points at the chat-completions root;
	// the OpenAI client appends "/chat/completions" and the vLLM
	// client does the same. The same LLM_BASE_URL works for OpenAI's
	// hosted endpoint (https://api.openai.com/v1) and a self-hosted
	// vLLM (http://vllm:8000/v1) — the wire contract is identical.
	//
	// LLM_API_KEY is required for the OpenAI provider (OpenAI proper
	// refuses empty bearer) but optional for the vLLM provider
	// (vLLM started without --api-key accepts any Authorization
	// header, including none). The placeholder blocklist below
	// catches the common "I committed my .env with a literal
	// 'your-key'" foot-gun.
	//
	// LLM_ALLOW_PRIVATE_URL relaxes the SSRF guard on LLM_BASE_URL
	// (loopback / RFC1918 / link-local / multicast / unspecified are
	// refused by default). Required for dev compose where the vLLM
	// container lives on a private container-network address, and
	// for any self-hosted deployment that isn't fronted by a public
	// hostname.
	//
	// LLM_SYSTEM_PROMPT is server-controlled and prepended to every
	// request; clients cannot override it (the chat service rejects
	// role:"system" messages from the wire). When empty, the chat
	// service falls back to a hard-coded default.
	//
	// LLM_MAX_HISTORY_MESSAGES / LLM_MAX_MESSAGE_CHARS cap the
	// per-request history the client may send. LLM_MAX_TOKENS caps
	// the upstream completion size. LLM_TIMEOUT is the per-call
	// socket deadline; LLM_MAX_STREAM_DURATION is the hard wall-clock
	// cap on a single stream — anything longer is force-closed.
	LLMEnabled            bool   `mapstructure:"LLM_ENABLED"`
	LLMProvider           string `mapstructure:"LLM_PROVIDER"`
	LLMBaseURL            string `mapstructure:"LLM_BASE_URL"`
	LLMAPIKey             string `mapstructure:"LLM_API_KEY"`
	LLMModel              string `mapstructure:"LLM_MODEL"`
	LLMTimeout            string `mapstructure:"LLM_TIMEOUT"`
	LLMMaxTokens          int    `mapstructure:"LLM_MAX_TOKENS"`
	LLMSystemPrompt       string `mapstructure:"LLM_SYSTEM_PROMPT"`
	LLMMaxHistoryMessages int    `mapstructure:"LLM_MAX_HISTORY_MESSAGES"`
	LLMMaxMessageChars    int    `mapstructure:"LLM_MAX_MESSAGE_CHARS"`
	LLMMaxStreamDuration  string `mapstructure:"LLM_MAX_STREAM_DURATION"`
	LLMAllowPrivateURL    bool   `mapstructure:"LLM_ALLOW_PRIVATE_URL"`

	// LLM failover. LLM_FALLBACK_* is the secondary slot consumed by
	// llm.Router when LLM_FALLBACK_BASE_URL is non-empty. The
	// fallback's provider type is implicit (the opposite of
	// LLM_PROVIDER — today only openai + vllm exist). LLM_FALLBACK_MODEL
	// is optional; the wiring site falls back to LLM_MODEL when empty.
	// LLM_PROBE_TIMEOUT caps the per-request GET /v1/models probe the
	// Router fires before each chat call (default 2s; should stay
	// well under LLM_TIMEOUT to leave room for the actual chat).
	LLMFallbackBaseURL string `mapstructure:"LLM_FALLBACK_BASE_URL"`
	LLMFallbackAPIKey  string `mapstructure:"LLM_FALLBACK_API_KEY"`
	LLMFallbackModel   string `mapstructure:"LLM_FALLBACK_MODEL"`
	LLMProbeTimeout    string `mapstructure:"LLM_PROBE_TIMEOUT"`

	// Embeddings + RAG over feeds. LLM_EMBEDDING_MODEL is the model
	// the rag package sends to /v1/embeddings; default empty means
	// fall back to LLM_MODEL (single-model deployments reuse the
	// chat model for embeddings). EMBEDDING_DIMENSIONS is the
	// vector dim the schema is built for; a model with a different
	// dim requires a schema migration AND setting this to match.
	// A startup-time schema-drift check in cmd/api/main.go warns
	// loudly when the live column dim disagrees.
	//
	// RAG is gated on LLM_ENABLED=true — no separate RAG_ENABLED.
	// The chat endpoint exposes the rag:true flag regardless; when
	// LLM is disabled the server simply runs without a provider
	// client and the chat service ignores the flag (defensive no-op).
	LLMEmbeddingModel        string `mapstructure:"LLM_EMBEDDING_MODEL"`
	EmbeddingDimensions      int    `mapstructure:"EMBEDDING_DIMENSIONS"`
	RAGTopK                  int    `mapstructure:"RAG_TOP_K"`
	RAGMaxContextChars       int    `mapstructure:"RAG_MAX_CONTEXT_CHARS"`
	RAGMaxBackfillPerRequest int    `mapstructure:"RAG_MAX_BACKFILL_PER_REQUEST"`

	// MCP server (Model Context Protocol) for the feed domain.
	// MCP_ENABLED is the master switch. When false (default) no
	// route is mounted and no mcp-go server is constructed — a
	// deployment that doesn't expose feeds to external agents pays
	// nothing. MCP_PATH is the URL prefix the Streamable HTTP server
	// is mounted under (default /mcp). MCP_MAX_BODY_BYTES is the
	// inner per-handler body cap; the outer router-level 1 MiB cap
	// (cmd/api/main.go maxBodyBytesLimit) still fires first.
	//
	// The default-off toggle mirrors LLM_ENABLED — same shape, same
	// fail-closed semantics, same call-site gating. JWT bearer auth
	// is required and is provided by the same middleware used by the
	// REST API; per-owner scoping is enforced inside feed.Service.
	MCPEnabled      bool   `mapstructure:"MCP_ENABLED"`
	MCPPath         string `mapstructure:"MCP_PATH"`
	MCPMaxBodyBytes int64  `mapstructure:"MCP_MAX_BODY_BYTES"`
}

func Load() (*Config, error) {
	setDefaults()

	// Explicitly bind every env-sourced key. viper.AutomaticEnv() only
	// checks env vars for keys already known to viper (via SetDefault,
	// BindEnv, or a successful ReadInConfig). In Docker there's no .env
	// file next to the binary, so without BindEnv the env vars are
	// silently ignored and DB_URL comes back empty.
	bindEnvVars()

	viper.AutomaticEnv()
	// Allow environment variables to override config file (e.g.,
	// DB_URL instead of db_url).
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	// Optionally load from .env file for local development. Missing
	// files are expected in production and ignored.
	viper.SetConfigFile(".env")
	viper.SetConfigType("env")
	if err := viper.ReadInConfig(); err != nil {
		// It's okay if .env doesn't exist in production
	}

	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, err
	}

	if err := validateDurations(&cfg); err != nil {
		return nil, err
	}

	// SECURITY.md M9 fail-closed: an empty DB_URL reaches pgxpool as
	// an unusable connection string and the first request would 500
	// instead of the backend refusing to start. The prod compose
	// ships with DB_URL="" in environment: and relies on
	// docker-entrypoint.sh to fill it from /run/secrets/db_url —
	// if the shim is bypassed (or the secret file is missing) we
	// must refuse to start here, not later. Tested by
	// TestLoadRejectsEmptyDBURL in config_test.go.
	if cfg.DBURL == "" {
		return nil, fmt.Errorf("DB_URL is required")
	}

	// Cross-field validation: enabling the cache requires a URL to connect to.
	if cfg.RedisEnabled && cfg.RedisURL == "" {
		return nil, fmt.Errorf("REDIS_URL is required when REDIS_ENABLED=true")
	}

	if auth.IsDefault(cfg.JWTSecret) {
		return nil, fmt.Errorf("JWT_SECRET must not be the default placeholder")
	}
	if len(cfg.JWTSecret) < auth.MinSecretBytes {
		return nil, fmt.Errorf("JWT_SECRET must be at least %d bytes", auth.MinSecretBytes)
	}

	if cfg.LLMEnabled {
		if err := validateLLMConfig(&cfg); err != nil {
			return nil, err
		}
	}

	if cfg.MCPEnabled {
		if err := validateMCPConfig(&cfg); err != nil {
			return nil, err
		}
	}

	return &cfg, nil
}

// setDefaults installs the viper defaults. Extracted so Load stays
// under the cyclomatic-complexity budget and the per-section defaults
// are documented as a single block.
func setDefaults() {
	viper.SetDefault("PORT", "8080")
	viper.SetDefault("MIGRATION_PATH", "file://migrations")
	viper.SetDefault("JWT_SECRET", "your-default-secret-key-change-it-in-prod")

	// JWT TTLs. Access is intentionally short (15m default) so a
	// stolen access token is replaced within one rotation cycle of
	// any active refresh token; refresh is the long-lived bearer
	// (168h = 7d default).
	viper.SetDefault("JWT_ACCESS_TTL", "15m")
	viper.SetDefault("JWT_REFRESH_TTL", "168h")

	// Database Connection Pool Defaults
	viper.SetDefault("DB_MAX_OPEN_CONNS", 25)
	viper.SetDefault("DB_MAX_IDLE_CONNS", 10)
	viper.SetDefault("DB_CONN_MAX_LIFETIME", "1h")
	viper.SetDefault("DB_CONN_MAX_IDLE_TIME", "30m")

	// Cache defaults — caching is opt-in. Set REDIS_ENABLED=true to
	// enable. REDIS_URL has no default: when REDIS_ENABLED=true it
	// must come from env, and when REDIS_ENABLED=false it's unused. A
	// blanket default would mask the cross-field validation below.
	viper.SetDefault("REDIS_ENABLED", false)
	viper.SetDefault("CACHE_TTL", "5m")

	// CORS allowlist default: empty. Deny-by-default — every
	// cross-origin request from a browser is rejected unless the
	// operator has set CORS_ALLOWED_ORIGINS to an explicit list (or,
	// for fully public credential-less APIs, to "*"). The same-origin
	// Vite proxy in development keeps the SPA unaffected by this
	// default.
	viper.SetDefault("CORS_ALLOWED_ORIGINS", "")

	// Cookie-based auth was retired in favour of Bearer tokens (see
	// FUTURE.md §4). The legacy COOKIE_* env vars are no longer
	// consumed — they would silently produce zero values if a stale
	// deployment kept them in its env file. The startup does not
	// fail-closed on this; operators should drop the stale vars when
	// upgrading.

	// OpenTelemetry tracing defaults. Disabled by default — every
	// tracer.Start becomes a no-op and unit tests / testcontainers
	// pay nothing. OTLP endpoint default points at the host loopback
	// so a developer running `jaegertracing/jaeger` (v2) locally can
	// flip OTEL_ENABLED=true without further config.
	viper.SetDefault("OTEL_ENABLED", false)
	viper.SetDefault("OTEL_SERVICE_NAME", "nyx-backend")
	viper.SetDefault("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")

	// LLM defaults. LLM_ENABLED is the master switch — false means
	// no route is registered and no client is constructed at
	// startup. LLM_PROVIDER picks the client implementation; "openai"
	// is the default so existing deployments don't need a new env
	// var. The other defaults (timeouts, caps) only matter when
	// LLM_ENABLED=true; they're installed so an operator can flip
	// LLM_ENABLED=true without also setting every limit.
	viper.SetDefault("LLM_ENABLED", false)
	viper.SetDefault("LLM_PROVIDER", "openai")
	viper.SetDefault("LLM_TIMEOUT", "60s")
	viper.SetDefault("LLM_MAX_TOKENS", 1024)
	viper.SetDefault("LLM_MAX_HISTORY_MESSAGES", 50)
	viper.SetDefault("LLM_MAX_MESSAGE_CHARS", 32768)
	viper.SetDefault("LLM_MAX_STREAM_DURATION", "10m")
	viper.SetDefault("LLM_ALLOW_PRIVATE_URL", false)
	// LLM_FALLBACK_* default to empty → no fallback configured →
	// Router is skipped entirely (single-provider passthrough).
	// LLM_PROBE_TIMEOUT caps the per-request /v1/models probe the
	// Router fires; 2s leaves room for the chat itself under a
	// typical 60s LLM_TIMEOUT.
	viper.SetDefault("LLM_PROBE_TIMEOUT", "2s")
	// LLM_SYSTEM_PROMPT is left empty by default — the chat
	// service falls back to a hard-coded "feed catalog
	// assistant" prompt when the operator hasn't customised it.

	// Embeddings + RAG defaults. LLM_EMBEDDING_MODEL defaults empty
	// so cmd/api/main.go can fall back to LLM_MODEL at wiring time.
	// EMBEDDING_DIMENSIONS defaults to 1536 (text-embedding-3-small)
	// and MUST match the feed_embeddings.embedding column dim — the
	// schema migration hardcodes 1536. Operators switching to a
	// different-dim model must write a new migration AND set
	// EMBEDDING_DIMENSIONS to match.
	viper.SetDefault("LLM_EMBEDDING_MODEL", "")
	viper.SetDefault("EMBEDDING_DIMENSIONS", 1536)
	viper.SetDefault("RAG_TOP_K", 5)
	viper.SetDefault("RAG_MAX_CONTEXT_CHARS", 4000)
	viper.SetDefault("RAG_MAX_BACKFILL_PER_REQUEST", 20)

	// MCP defaults. MCP_ENABLED=false (default) means no /mcp
	// route is mounted and no mcp-go server is constructed.
	// MCP_PATH default /mcp matches the standard convention; the
	// MCP spec recommends a non-versioned path and most MCP clients
	// (Claude Code, Claude Desktop) discover the server at /mcp.
	// MCP_MAX_BODY_BYTES 64 KiB matches the chat per-handler cap;
	// tool args are tiny so the cap catches a buggy agent without
	// rejecting legitimate inputs.
	viper.SetDefault("MCP_ENABLED", false)
	viper.SetDefault("MCP_PATH", "/mcp")
	viper.SetDefault("MCP_MAX_BODY_BYTES", 65536)
}

// bindEnvVars explicitly wires every env-sourced viper key. See Load
// for why this is necessary (AutomaticEnv alone doesn't pick up
// keys that haven't been registered via SetDefault / BindEnv).
func bindEnvVars() {
	for _, key := range []string{
		"DB_URL", "JWT_SECRET", "JWT_ACCESS_TTL", "JWT_REFRESH_TTL",
		"PORT", "MIGRATION_PATH",
		"DB_MAX_OPEN_CONNS", "DB_MAX_IDLE_CONNS",
		"DB_CONN_MAX_LIFETIME", "DB_CONN_MAX_IDLE_TIME",
		"REDIS_URL", "REDIS_ENABLED", "CACHE_TTL",
		"CORS_ALLOWED_ORIGINS",
		"OTEL_ENABLED", "OTEL_SERVICE_NAME",
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"LLM_ENABLED", "LLM_PROVIDER", "LLM_BASE_URL", "LLM_API_KEY", "LLM_MODEL",
		"LLM_TIMEOUT", "LLM_MAX_TOKENS", "LLM_SYSTEM_PROMPT",
		"LLM_MAX_HISTORY_MESSAGES", "LLM_MAX_MESSAGE_CHARS",
		"LLM_MAX_STREAM_DURATION", "LLM_ALLOW_PRIVATE_URL",
		"LLM_FALLBACK_BASE_URL", "LLM_FALLBACK_API_KEY", "LLM_FALLBACK_MODEL",
		"LLM_PROBE_TIMEOUT",
		"LLM_EMBEDDING_MODEL", "EMBEDDING_DIMENSIONS",
		"RAG_TOP_K", "RAG_MAX_CONTEXT_CHARS", "RAG_MAX_BACKFILL_PER_REQUEST",
		"MCP_ENABLED", "MCP_PATH", "MCP_MAX_BODY_BYTES",
	} {
		_ = viper.BindEnv(key)
	}
}

// validateDurations parses every duration field and enforces the
// JWT TTL relationship (refresh must outlast access). Extracted from
// Load so the public function stays under the cyclomatic-complexity
// budget and the duration rules are documented as a single block.
func validateDurations(cfg *Config) error {
	if _, err := time.ParseDuration(cfg.DBConnMaxLifetime); err != nil {
		return fmt.Errorf("invalid DB_CONN_MAX_LIFETIME: %w", err)
	}
	if _, err := time.ParseDuration(cfg.DBConnMaxIdleTime); err != nil {
		return fmt.Errorf("invalid DB_CONN_MAX_IDLE_TIME: %w", err)
	}
	if _, err := time.ParseDuration(cfg.CacheTTL); err != nil {
		return fmt.Errorf("invalid CACHE_TTL: %w", err)
	}

	accessDur, err := time.ParseDuration(cfg.JWTAccessTTL)
	if err != nil {
		return fmt.Errorf("invalid JWT_ACCESS_TTL: %w", err)
	}
	if accessDur <= 0 {
		return fmt.Errorf("JWT_ACCESS_TTL must be positive, got %v", accessDur)
	}
	refreshDur, err := time.ParseDuration(cfg.JWTRefreshTTL)
	if err != nil {
		return fmt.Errorf("invalid JWT_REFRESH_TTL: %w", err)
	}
	if refreshDur <= 0 {
		return fmt.Errorf("JWT_REFRESH_TTL must be positive, got %v", refreshDur)
	}
	if refreshDur <= accessDur {
		return fmt.Errorf(
			"JWT_REFRESH_TTL (%v) must be greater than JWT_ACCESS_TTL (%v)",
			refreshDur, accessDur,
		)
	}

	// LLM durations — only parsed when LLM is enabled to keep the
	// default-off path free of "set me to use me" errors.
	if cfg.LLMEnabled {
		if _, err := time.ParseDuration(cfg.LLMTimeout); err != nil {
			return fmt.Errorf("invalid LLM_TIMEOUT: %w", err)
		}
		if _, err := time.ParseDuration(cfg.LLMMaxStreamDuration); err != nil {
			return fmt.Errorf("invalid LLM_MAX_STREAM_DURATION: %w", err)
		}
		if _, err := time.ParseDuration(cfg.LLMProbeTimeout); err != nil {
			return fmt.Errorf("invalid LLM_PROBE_TIMEOUT: %w", err)
		}
	}
	return nil
}

// validateLLMConfig enforces the cross-field requirements for
// turning LLM_ENABLED on. The rules:
//   - Provider must be one of {"openai", "vllm"} — anything else
//     would silently fall back to the OpenAI client at runtime.
//   - BaseURL and Model must be non-empty (no defaults).
//   - APIKey is required for OpenAI (it rejects empty bearer)
//     but optional for vLLM (vLLM started without --api-key
//     accepts any Authorization header, including none).
//   - APIKey (when set) must not be a placeholder an operator
//     might leave in by accident (case-insensitive contains-match
//     against a short blocklist). The check is intentionally cheap —
//     it catches the common "I committed my .env with a literal
//     'your-key'" foot-gun, not a determined attacker.
//   - BaseURL scheme must be http or https; userinfo is rejected;
//     private/restricted hosts (loopback, RFC1918, link-local,
//     multicast, unspecified) are rejected unless the operator
//     opts in via LLM_ALLOW_PRIVATE_URL=true. The per-dial
//     re-check happens inside the vLLM provider's
//     http.Transport.DialContext; this is the startup guard.
//   - System prompt length is capped at 8 KiB to bound the input
//     to every chat completion.
//   - Fallback (LLM_FALLBACK_BASE_URL non-empty) must satisfy the
//     same scheme/host/SSRF rules. Fallback provider type is the
//     opposite of LLM_PROVIDER; OpenAI fallback requires a
//     non-placeholder key, vLLM fallback allows empty key.
//     LLM_FALLBACK_MODEL is optional (defaults to LLM_MODEL).
//
// Called only when LLMEnabled is true so the default-off path
// stays free of these requirements.
func validateLLMConfig(cfg *Config) error {
	switch cfg.LLMProvider {
	case LLMProviderOpenAI, LLMProviderVLLM:
	case "":
		// Empty means viper default didn't apply — treat as the
		// canonical "openai" so a misconfigured deployment fails
		// loudly here rather than silently routing to OpenAI.
		return fmt.Errorf("LLM_PROVIDER must be one of {%s, %s}, got empty", LLMProviderOpenAI, LLMProviderVLLM)
	default:
		return fmt.Errorf("LLM_PROVIDER must be one of {%s, %s}, got %q", LLMProviderOpenAI, LLMProviderVLLM, cfg.LLMProvider)
	}
	if cfg.LLMBaseURL == "" {
		return fmt.Errorf("LLM_BASE_URL is required when LLM_ENABLED=true")
	}
	if cfg.LLMModel == "" {
		return fmt.Errorf("LLM_MODEL is required when LLM_ENABLED=true")
	}
	if err := validateLLMBaseURL(cfg.LLMBaseURL, cfg.LLMAllowPrivateURL); err != nil {
		return err
	}
	// OpenAI rejects an empty bearer; vLLM doesn't. Only the OpenAI
	// path enforces a non-empty key.
	if cfg.LLMProvider == LLMProviderOpenAI {
		if cfg.LLMAPIKey == "" {
			return fmt.Errorf("LLM_API_KEY is required when LLM_ENABLED=true and LLM_PROVIDER=openai")
		}
		if err := checkNotPlaceholderKey(cfg.LLMAPIKey, "LLM_API_KEY"); err != nil {
			return err
		}
	}

	// Fallback slot — only validated when LLM_FALLBACK_BASE_URL is
	// non-empty. The fallback's provider type is the OPPOSITE of
	// the primary's; with only two impls today (openai, vllm) that's
	// unambiguous. If a third provider is added, this rule needs
	// either a sibling LLM_FALLBACK_PROVIDER env or a richer router.
	if cfg.LLMFallbackBaseURL != "" {
		if err := validateLLMBaseURL(cfg.LLMFallbackBaseURL, cfg.LLMAllowPrivateURL); err != nil {
			return err
		}
		fallbackIsOpenAI := cfg.LLMProvider == LLMProviderVLLM
		if fallbackIsOpenAI {
			if cfg.LLMFallbackAPIKey == "" {
				return fmt.Errorf("LLM_FALLBACK_API_KEY is required when LLM_PROVIDER=vllm (fallback is OpenAI)")
			}
			if err := checkNotPlaceholderKey(cfg.LLMFallbackAPIKey, "LLM_FALLBACK_API_KEY"); err != nil {
				return err
			}
		}
		// vLLM fallback: LLM_FALLBACK_API_KEY optional — empty is
		// the standard vLLM-without---api-key deployment.
	}

	if len(cfg.LLMSystemPrompt) > 8192 {
		return fmt.Errorf("LLM_SYSTEM_PROMPT must be at most 8192 bytes, got %d", len(cfg.LLMSystemPrompt))
	}

	// LLM_EMBEDDING_MODEL is optional (defaults to LLM_MODEL via
	// cmd/api/main.go), but when set it must not be a placeholder.
	// Reuses the chat key blocklist — the foot-gun is the same:
	// operator committed a literal "your-key" into .env.
	if cfg.LLMEmbeddingModel != "" {
		if err := checkNotPlaceholderKey(cfg.LLMEmbeddingModel, "LLM_EMBEDDING_MODEL"); err != nil {
			return err
		}
	}
	return nil
}

// checkNotPlaceholderKey rejects the common "I committed my .env
// with a literal placeholder" foot-gun. Case-insensitive
// contains-match against a short blocklist — the check is
// intentionally cheap and catches careless operators, not
// determined attackers.
func checkNotPlaceholderKey(key, fieldName string) error {
	lower := strings.ToLower(key)
	for _, placeholder := range []string{"changeme", "your-key", "sk-xxx", "xxx", "test-key"} {
		if strings.Contains(lower, placeholder) {
			return fmt.Errorf("%s looks like a placeholder (%q); refusing to start — set a real key", fieldName, placeholder)
		}
	}
	return nil
}

// validateMCPConfig enforces the cross-field requirements for
// turning MCP_ENABLED on. The rules:
//   - MCPPath must start with "/" and must not contain "?" or "#"
//     (fragment / query in a route path is a misconfiguration).
//   - MCPMaxBodyBytes must be in [1024, 1048576] — the upper bound
//     matches the outer router cap (1 MiB) so an inner cap larger
//     than the outer is impossible.
//
// Called only when MCPEnabled is true so the default-off path
// stays free of these requirements. Auth uses the project's
// existing JWT bearer middleware (no companion secrets required).
func validateMCPConfig(cfg *Config) error {
	if cfg.MCPPath == "" || cfg.MCPPath[0] != '/' {
		return fmt.Errorf("MCP_PATH must start with '/', got %q", cfg.MCPPath)
	}
	for _, bad := range []string{"?", "#"} {
		if strings.Contains(cfg.MCPPath, bad) {
			return fmt.Errorf("MCP_PATH must not contain %q", bad)
		}
	}
	if cfg.MCPMaxBodyBytes < 1024 {
		return fmt.Errorf("MCP_MAX_BODY_BYTES must be at least 1024, got %d", cfg.MCPMaxBodyBytes)
	}
	if cfg.MCPMaxBodyBytes > 1048576 {
		return fmt.Errorf("MCP_MAX_BODY_BYTES must be at most 1048576 (1 MiB), got %d", cfg.MCPMaxBodyBytes)
	}
	return nil
}

// validateLLMBaseURL is the startup-time SSRF guard. It refuses:
//   - malformed URLs (url.Parse fails)
//   - schemes other than http / https
//   - URLs with embedded userinfo (http://user:pw@host/) —
//     userinfo is never a legitimate LLM URL component and
//     silently dropping it would let a misconfigured client
//     appear to authenticate as a different principal
//   - hosts that resolve to loopback, RFC1918, link-local,
//     multicast, or unspecified IPs — unless the operator
//     opts in via LLM_ALLOW_PRIVATE_URL=true
//
// The vLLM provider repeats the IP-class check inside its
// http.Transport.DialContext so a DNS rebinding attack can't
// swap a public hostname's answer for a private IP between
// startup and the first dial. The OpenAI provider (which uses
// the sashabaranov SDK) inherits the SDK's default transport
// with no per-dial check; this is a known residual risk and
// is documented in SECURITY.md.
func validateLLMBaseURL(rawURL string, allowPrivate bool) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("LLM_BASE_URL is not a valid URL: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("LLM_BASE_URL scheme must be http or https, got %q", u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("LLM_BASE_URL must not contain userinfo (user:password@host)")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("LLM_BASE_URL is missing a host")
	}
	if allowPrivate {
		return nil
	}
	if err := checkHostNotPrivate(host); err != nil {
		return fmt.Errorf("%w (set LLM_ALLOW_PRIVATE_URL=true to override)", err)
	}
	return nil
}

// checkHostNotPrivate resolves the host and refuses any address in
// the loopback, RFC1918, link-local, multicast, or unspecified
// classes. Mirrors the check inside internal/llm/vllm's
// DialContext so an operator gets a clear error at startup
// instead of an opaque 502 at request time.
func checkHostNotPrivate(host string) error {
	if ip := net.ParseIP(host); ip != nil {
		if refused, reason := ipClassRefused(ip); refused {
			return fmt.Errorf("LLM_BASE_URL points at a private/restricted address (%s)", reason)
		}
		return nil
	}
	addrs, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("LLM_BASE_URL DNS lookup failed for %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("LLM_BASE_URL DNS lookup returned no addresses for %q", host)
	}
	for _, ip := range addrs {
		if refused, reason := ipClassRefused(ip); refused {
			return fmt.Errorf("LLM_BASE_URL resolves to a private/restricted address (%s)", reason)
		}
	}
	return nil
}

// ipClassRefused returns (true, reason) when the IP belongs to a
// class the project refuses by default. Uses net.IP's
// classification methods (Go 1.17+) so RFC1918 + ULA +
// link-local + IPv4-mapped IPv6 are all covered without rolling
// CIDR checks by hand.
func ipClassRefused(ip net.IP) (bool, string) {
	switch {
	case ip.IsUnspecified():
		return true, "unspecified (0.0.0.0 / ::)"
	case ip.IsLoopback():
		return true, "loopback (127.0.0.0/8 / ::1)"
	case ip.IsPrivate():
		// Covers RFC1918 (10/8, 172.16/12, 192.168/16) and ULA (fc00::/7).
		return true, "private (RFC1918 / ULA)"
	case ip.IsLinkLocalUnicast():
		return true, "link-local unicast (169.254/16 / fe80::/10)"
	case ip.IsLinkLocalMulticast():
		return true, "link-local multicast"
	case ip.IsInterfaceLocalMulticast():
		return true, "interface-local multicast (ff01::/16)"
	case ip.IsMulticast():
		// Site-local / org-local multicast — operators don't
		// legitimately point an LLM URL at a multicast group.
		return true, "multicast"
	}
	return false, ""
}
