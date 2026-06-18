package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

type Config struct {
	DBURL          string `mapstructure:"DB_URL"`
	JWTSecret      string `mapstructure:"JWT_SECRET"`
	Port           string `mapstructure:"PORT"`
	GinMode        string `mapstructure:"GIN_MODE"`
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
}

func Load() (*Config, error) {
	viper.SetDefault("PORT", "8080")
	viper.SetDefault("GIN_MODE", "release")
	viper.SetDefault("MIGRATION_PATH", "file://migrations")
	viper.SetDefault("JWT_SECRET", "your-default-secret-key-change-it-in-prod")

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

	// Explicitly bind every env-sourced key. viper.AutomaticEnv() only checks
	// env vars for keys already known to viper (via SetDefault, BindEnv, or a
	// successful ReadInConfig). In Docker there's no .env file next to the
	// binary, so without BindEnv the env vars are silently ignored and
	// DB_URL comes back empty.
	for _, key := range []string{
		"DB_URL", "JWT_SECRET", "PORT", "GIN_MODE", "MIGRATION_PATH",
		"DB_MAX_OPEN_CONNS", "DB_MAX_IDLE_CONNS",
		"DB_CONN_MAX_LIFETIME", "DB_CONN_MAX_IDLE_TIME",
		"REDIS_URL", "REDIS_ENABLED", "CACHE_TTL",
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

	// Cross-field validation: enabling the cache requires a URL to connect to.
	if cfg.RedisEnabled && cfg.RedisURL == "" {
		return nil, fmt.Errorf("REDIS_URL is required when REDIS_ENABLED=true")
	}

	return &cfg, nil
}
