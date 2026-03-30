package config

import (
	"strings"

	"github.com/spf13/viper"
)

type Config struct {
	DBURL          string `mapstructure:"DB_URL"`
	JWTSecret      string `mapstructure:"JWT_SECRET"`
	Port           string `mapstructure:"PORT"`
	GinMode        string `mapstructure:"GIN_MODE"`
	MigrationPath  string `mapstructure:"MIGRATION_PATH"`
}

func Load() (*Config, error) {
	viper.SetDefault("PORT", "8080")
	viper.SetDefault("GIN_MODE", "release")
	viper.SetDefault("MIGRATION_PATH", "file://migrations")
	viper.SetDefault("JWT_SECRET", "your-default-secret-key-change-it-in-prod")

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

	return &cfg, nil
}
