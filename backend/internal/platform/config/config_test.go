package config

import (
	"testing"
	"time"
)

// TestLoadFromEnv verifies that Load() reads every required env var.
// This is the regression test for the Docker "DB_URL environment variable
// is required" failure: viper.AutomaticEnv() silently ignores env vars for
// keys it doesn't already know about, so without explicit BindEnv the env
// vars are dropped on the floor.
func TestLoadFromEnv(t *testing.T) {
	t.Setenv("DB_URL", "postgres://user:pass@db:5432/nyx?sslmode=disable")
	t.Setenv("JWT_SECRET", "test-secret")
	t.Setenv("REDIS_ENABLED", "true")
	t.Setenv("REDIS_URL", "redis://redis:6379")
	t.Setenv("CACHE_TTL", "10m")
	t.Setenv("DB_CONN_MAX_LIFETIME", "2h")
	t.Setenv("DB_CONN_MAX_IDLE_TIME", "15m")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.DBURL != "postgres://user:pass@db:5432/nyx?sslmode=disable" {
		t.Errorf("DBURL = %q, want from env", cfg.DBURL)
	}
	if cfg.JWTSecret != "test-secret" {
		t.Errorf("JWTSecret = %q, want %q", cfg.JWTSecret, "test-secret")
	}
	if !cfg.RedisEnabled {
		t.Error("RedisEnabled = false, want true from env")
	}
	if cfg.RedisURL != "redis://redis:6379" {
		t.Errorf("RedisURL = %q, want from env", cfg.RedisURL)
	}
	if cfg.CacheTTL != "10m" {
		t.Errorf("CacheTTL = %q, want %q", cfg.CacheTTL, "10m")
	}
	if cfg.DBConnMaxLifetime != "2h" {
		t.Errorf("DBConnMaxLifetime = %q, want %q", cfg.DBConnMaxLifetime, "2h")
	}
	if cfg.DBConnMaxIdleTime != "15m" {
		t.Errorf("DBConnMaxIdleTime = %q, want %q", cfg.DBConnMaxIdleTime, "15m")
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	// Isolate the test from any ambient env vars.
	t.Setenv("DB_URL", "postgres://x")
	t.Setenv("JWT_SECRET", "x")
	t.Setenv("REDIS_ENABLED", "")
	t.Setenv("PORT", "")
	t.Setenv("GIN_MODE", "")
	t.Setenv("DB_MAX_OPEN_CONNS", "")
	t.Setenv("DB_MAX_IDLE_CONNS", "")
	t.Setenv("DB_CONN_MAX_LIFETIME", "")
	t.Setenv("DB_CONN_MAX_IDLE_TIME", "")
	t.Setenv("CACHE_TTL", "")
	t.Setenv("REDIS_URL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Port != "8080" {
		t.Errorf("Port default = %q, want %q", cfg.Port, "8080")
	}
	if cfg.GinMode != "release" {
		t.Errorf("GinMode default = %q, want %q", cfg.GinMode, "release")
	}
	if cfg.DBMaxOpenConns != 25 {
		t.Errorf("DBMaxOpenConns default = %d, want 25", cfg.DBMaxOpenConns)
	}
	if cfg.DBMaxIdleConns != 10 {
		t.Errorf("DBMaxIdleConns default = %d, want 10", cfg.DBMaxIdleConns)
	}
	if cfg.CacheTTL != "5m" {
		t.Errorf("CacheTTL default = %q, want %q", cfg.CacheTTL, "5m")
	}
	if cfg.RedisEnabled {
		t.Error("RedisEnabled default = true, want false")
	}
}

func TestLoadRejectsRedisEnabledWithoutURL(t *testing.T) {
	t.Setenv("DB_URL", "postgres://x")
	t.Setenv("JWT_SECRET", "x")
	t.Setenv("REDIS_ENABLED", "true")
	t.Setenv("REDIS_URL", "")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want REDIS_URL-required error")
	}
}

func TestLoadRejectsBadDurations(t *testing.T) {
	t.Setenv("DB_URL", "postgres://x")
	t.Setenv("JWT_SECRET", "x")
	t.Setenv("DB_CONN_MAX_LIFETIME", "not-a-duration")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want duration parse error")
	}
}

// Sanity check on the duration string-to-time.Duration conversion that Load
// uses internally, so a typo in a default surfaces a clear test failure
// instead of a runtime DB connection tuning surprise.
func TestLoadDurationsAreValid(t *testing.T) {
	t.Setenv("DB_URL", "postgres://x")
	t.Setenv("JWT_SECRET", "x")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	for _, d := range []string{cfg.DBConnMaxLifetime, cfg.DBConnMaxIdleTime, cfg.CacheTTL} {
		if _, err := time.ParseDuration(d); err != nil {
			t.Errorf("duration %q failed to parse: %v", d, err)
		}
	}
}