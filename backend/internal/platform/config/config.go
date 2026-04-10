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

	return &cfg, nil
}
