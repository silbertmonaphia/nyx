package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
	"nyx/internal/platform/auth"
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
	// LLM_BASE_URL, LLM_API_KEY, and LLM_MODEL. LLM_BASE_URL points
	// at the chat-completions root (the OpenAI client appends
	// "/chat/completions"); the same field works for OpenAI's hosted
	// endpoint (https://api.openai.com/v1) and a self-hosted vLLM
	// (http://vllm.internal:8000/v1) — the wire contract is identical.
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
	LLMBaseURL            string `mapstructure:"LLM_BASE_URL"`
	LLMAPIKey             string `mapstructure:"LLM_API_KEY"`
	LLMModel              string `mapstructure:"LLM_MODEL"`
	LLMTimeout            string `mapstructure:"LLM_TIMEOUT"`
	LLMMaxTokens          int    `mapstructure:"LLM_MAX_TOKENS"`
	LLMSystemPrompt       string `mapstructure:"LLM_SYSTEM_PROMPT"`
	LLMMaxHistoryMessages int    `mapstructure:"LLM_MAX_HISTORY_MESSAGES"`
	LLMMaxMessageChars    int    `mapstructure:"LLM_MAX_MESSAGE_CHARS"`
	LLMMaxStreamDuration  string `mapstructure:"LLM_MAX_STREAM_DURATION"`
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
	// so a developer running `jaegertracing/all-in-one` locally can
	// flip OTEL_ENABLED=true without further config.
	viper.SetDefault("OTEL_ENABLED", false)
	viper.SetDefault("OTEL_SERVICE_NAME", "nyx-backend")
	viper.SetDefault("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")

	// LLM defaults. LLM_ENABLED is the master switch — false means
	// no route is registered and no client is constructed at
	// startup. The other defaults (timeouts, caps) only matter
	// when LLM_ENABLED=true; they're installed so an operator can
	// flip LLM_ENABLED=true without also setting every limit.
	viper.SetDefault("LLM_ENABLED", false)
	viper.SetDefault("LLM_TIMEOUT", "60s")
	viper.SetDefault("LLM_MAX_TOKENS", 1024)
	viper.SetDefault("LLM_MAX_HISTORY_MESSAGES", 50)
	viper.SetDefault("LLM_MAX_MESSAGE_CHARS", 32768)
	viper.SetDefault("LLM_MAX_STREAM_DURATION", "10m")
	// LLM_SYSTEM_PROMPT is left empty by default — the chat
	// service falls back to a hard-coded "movie catalog
	// assistant" prompt when the operator hasn't customised it.
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
		"LLM_ENABLED", "LLM_BASE_URL", "LLM_API_KEY", "LLM_MODEL",
		"LLM_TIMEOUT", "LLM_MAX_TOKENS", "LLM_SYSTEM_PROMPT",
		"LLM_MAX_HISTORY_MESSAGES", "LLM_MAX_MESSAGE_CHARS",
		"LLM_MAX_STREAM_DURATION",
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
	}
	return nil
}

// validateLLMConfig enforces the cross-field requirements for
// turning LLM_ENABLED on. The rules:
//   - BaseURL, APIKey, and Model must all be non-empty (no defaults).
//   - APIKey must not be a placeholder that an operator might leave
//     in by accident (case-insensitive contains-match against a
//     short blocklist). The check is intentionally cheap — it
//     catches the common "I committed my .env with a literal
//     'your-key'" foot-gun, not a determined attacker.
//   - System prompt length is capped at 8 KiB to bound the input to
//     every chat completion.
//
// Called only when LLMEnabled is true so the default-off path
// stays free of these requirements.
func validateLLMConfig(cfg *Config) error {
	if cfg.LLMBaseURL == "" {
		return fmt.Errorf("LLM_BASE_URL is required when LLM_ENABLED=true")
	}
	if cfg.LLMAPIKey == "" {
		return fmt.Errorf("LLM_API_KEY is required when LLM_ENABLED=true")
	}
	if cfg.LLMModel == "" {
		return fmt.Errorf("LLM_MODEL is required when LLM_ENABLED=true")
	}
	lower := strings.ToLower(cfg.LLMAPIKey)
	for _, placeholder := range []string{"changeme", "your-key", "sk-xxx", "xxx", "test-key"} {
		if strings.Contains(lower, placeholder) {
			return fmt.Errorf("LLM_API_KEY looks like a placeholder (%q); refusing to start — set a real key", placeholder)
		}
	}
	if len(cfg.LLMSystemPrompt) > 8192 {
		return fmt.Errorf("LLM_SYSTEM_PROMPT must be at most 8192 bytes, got %d", len(cfg.LLMSystemPrompt))
	}
	return nil
}
