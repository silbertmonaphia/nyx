package config

import (
	"fmt"
	"strings"
	"time"

	"nyx/internal/platform/auth"
	"github.com/spf13/viper"
)

type Config struct {
	DBURL          string `mapstructure:"DB_URL"`
	JWTSecret      string `mapstructure:"JWT_SECRET"`
	JWTAccessTTL   string `mapstructure:"JWT_ACCESS_TTL"`
	JWTRefreshTTL  string `mapstructure:"JWT_REFRESH_TTL"`
	Port           string `mapstructure:"PORT"`
	MigrationPath  string `mapstructure:"MIGRATION_PATH"`

	// Database Connection Pool
	DBMaxOpenConns    int    `mapstructure:"DB_MAX_OPEN_CONNS"`
	DBMaxIdleConns    int    `mapstructure:"DB_MAX_IDLE_CONNS"`
	DBConnMaxLifetime string `mapstructure:"DB_CONN_MAX_LIFETIME"`
	DBConnMaxIdleTime string `mapstructure:"DB_CONN_MAX_IDLE_TIME"`

	// Cache
	RedisURL     string `mapstructure:"REDIS_URL"`
	RedisEnabled bool   `mapstructure:"REDIS_ENABLED"`
	CacheTTL     string `mapstructure:"CACHE_TTL"`

	// CORS — comma-separated allowlist. Default "*" preserves the
	// gin-era permissive policy for local dev. Production should set
	// this to the explicit list of frontend origins (e.g.
	// "https://app.example.com,https://staging.example.com").
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
}

func Load() (*Config, error) {
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

	// Cache defaults — caching is opt-in. Set REDIS_ENABLED=true to enable.
	// REDIS_URL has no default: when REDIS_ENABLED=true it must come from env,
	// and when REDIS_ENABLED=false it's unused. A blanket default would mask
	// the cross-field validation below.
	viper.SetDefault("REDIS_ENABLED", false)
	viper.SetDefault("CACHE_TTL", "5m")

	// CORS allowlist default: "*" preserves the gin-era permissive policy
	// for local dev. Production must set CORS_ALLOWED_ORIGINS to a
	// comma-separated list of explicit origins.
	viper.SetDefault("CORS_ALLOWED_ORIGINS", "*")

	// OpenTelemetry tracing defaults. Disabled by default — every
	// tracer.Start becomes a no-op and unit tests / testcontainers
	// pay nothing. OTLP endpoint default points at the host loopback
	// so a developer running `jaegertracing/all-in-one` locally can
	// flip OTEL_ENABLED=true without further config.
	viper.SetDefault("OTEL_ENABLED", false)
	viper.SetDefault("OTEL_SERVICE_NAME", "nyx-backend")
	viper.SetDefault("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")

	// Explicitly bind every env-sourced key. viper.AutomaticEnv() only checks
	// env vars for keys already known to viper (via SetDefault, BindEnv, or a
	// successful ReadInConfig). In Docker there's no .env file next to the
	// binary, so without BindEnv the env vars are silently ignored and
	// DB_URL comes back empty.
	for _, key := range []string{
		"DB_URL", "JWT_SECRET", "JWT_ACCESS_TTL", "JWT_REFRESH_TTL",
		"PORT", "MIGRATION_PATH",
		"DB_MAX_OPEN_CONNS", "DB_MAX_IDLE_CONNS",
		"DB_CONN_MAX_LIFETIME", "DB_CONN_MAX_IDLE_TIME",
		"REDIS_URL", "REDIS_ENABLED", "CACHE_TTL",
		"CORS_ALLOWED_ORIGINS",
		"OTEL_ENABLED", "OTEL_SERVICE_NAME",
		"OTEL_EXPORTER_OTLP_ENDPOINT",
	} {
		_ = viper.BindEnv(key)
	}

	viper.AutomaticEnv()
	// Allow environment variables to override config file (e.g., DB_URL instead of db_url)
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	// Optionally load from .env file for local development
	viper.SetConfigFile(".env")
	viper.SetConfigType("env")
	if err := viper.ReadInConfig(); err != nil {
		// It's okay if .env doesn't exist in production
	}

	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, err
	}

	// Validate Durations
	if _, err := time.ParseDuration(cfg.DBConnMaxLifetime); err != nil {
		return nil, fmt.Errorf("invalid DB_CONN_MAX_LIFETIME: %w", err)
	}
	if _, err := time.ParseDuration(cfg.DBConnMaxIdleTime); err != nil {
		return nil, fmt.Errorf("invalid DB_CONN_MAX_IDLE_TIME: %w", err)
	}
	if _, err := time.ParseDuration(cfg.CacheTTL); err != nil {
		return nil, fmt.Errorf("invalid CACHE_TTL: %w", err)
	}

	accessDur, err := time.ParseDuration(cfg.JWTAccessTTL)
	if err != nil {
		return nil, fmt.Errorf("invalid JWT_ACCESS_TTL: %w", err)
	}
	if accessDur <= 0 {
		return nil, fmt.Errorf("JWT_ACCESS_TTL must be positive, got %v", accessDur)
	}
	refreshDur, err := time.ParseDuration(cfg.JWTRefreshTTL)
	if err != nil {
		return nil, fmt.Errorf("invalid JWT_REFRESH_TTL: %w", err)
	}
	if refreshDur <= 0 {
		return nil, fmt.Errorf("JWT_REFRESH_TTL must be positive, got %v", refreshDur)
	}
	if refreshDur <= accessDur {
		return nil, fmt.Errorf(
			"JWT_REFRESH_TTL (%v) must be greater than JWT_ACCESS_TTL (%v)",
			refreshDur, accessDur,
		)
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

	return &cfg, nil
}
