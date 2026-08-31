package config

import (
	"strings"
	"testing"
	"time"

	"nyx/internal/platform/auth"
)

// TestLoadFromEnv verifies that Load() reads every required env var.
// This is the regression test for the Docker "DB_URL environment variable
// is required" failure: viper.AutomaticEnv() silently ignores env vars for
// keys it doesn't already know about, so without explicit BindEnv the env
// vars are dropped on the floor.
func TestLoadFromEnv(t *testing.T) {
	t.Setenv("DB_URL", "postgres://user:pass@db:5432/nyx?sslmode=disable")
	t.Setenv("JWT_SECRET", auth.TestSecret)
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
	if cfg.JWTSecret != auth.TestSecret {
		t.Errorf("JWTSecret = %q, want %q", cfg.JWTSecret, auth.TestSecret)
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
	t.Setenv("JWT_SECRET", auth.TestSecret)
	t.Setenv("REDIS_ENABLED", "")
	t.Setenv("PORT", "")
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
	t.Setenv("JWT_SECRET", auth.TestSecret)
	t.Setenv("REDIS_ENABLED", "true")
	t.Setenv("REDIS_URL", "")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want REDIS_URL-required error")
	}
}

func TestLoadRejectsBadDurations(t *testing.T) {
	t.Setenv("DB_URL", "postgres://x")
	t.Setenv("JWT_SECRET", auth.TestSecret)
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
	t.Setenv("JWT_SECRET", auth.TestSecret)

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

func TestLoadRejectsDefaultJWTSecret(t *testing.T) {
	t.Setenv("DB_URL", "postgres://x")
	t.Setenv("JWT_SECRET", "your-default-secret-key-change-it-in-prod")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want JWT_SECRET default-placeholder rejection")
	}
	if !strings.Contains(err.Error(), "JWT_SECRET") {
		t.Errorf("Load() error = %v, want it to mention JWT_SECRET", err)
	}
}

func TestLoadRejectsShortJWTSecret(t *testing.T) {
	t.Setenv("DB_URL", "postgres://x")
	t.Setenv("JWT_SECRET", "tooshort")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want short-JWT_SECRET rejection")
	}
	if !strings.Contains(err.Error(), "JWT_SECRET") {
		t.Errorf("Load() error = %v, want it to mention JWT_SECRET", err)
	}
}

// TestLoadRejectsEmptyDBURL pins the SECURITY.md M9 fail-closed
// contract: the prod compose ships with `DB_URL=` (empty literal)
// in environment: and relies on docker-entrypoint.sh to fill it
// in from the mounted secret. If the shim is bypassed or the
// secret file is missing, the backend MUST refuse to start —
// starting with DB_URL="" would let pgxpool.ParseConfig("") silently
// open an unusable pool and the first request would 500. This test
// pins that the load step itself errors before any network call.
func TestLoadRejectsEmptyDBURL(t *testing.T) {
	// Isolate from any ambient DB_URL.
	t.Setenv("DB_URL", "")
	t.Setenv("JWT_SECRET", auth.TestSecret)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want empty-DB_URL rejection")
	}
	if !strings.Contains(err.Error(), "DB_URL") {
		t.Errorf("Load() error = %v, want it to mention DB_URL", err)
	}
}

// TestLoadRejectsEmptyJWTSecret is the M9 mirror of the test above
// for JWT_SECRET. The shim only exports JWT_SECRET if the secret
// file is non-empty — an empty or missing secret leaves the env
// var empty. The placeholder default would otherwise kick in via
// viper.SetDefault and pass IsDefault, but the empty string is NOT
// the placeholder literal, so we need a dedicated check. viper
// falls through to the empty string, then this test pins that the
// length check still fires.
func TestLoadRejectsEmptyJWTSecret(t *testing.T) {
	t.Setenv("DB_URL", "postgres://x")
	t.Setenv("JWT_SECRET", "")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want empty-JWT_SECRET rejection")
	}
	if !strings.Contains(err.Error(), "JWT_SECRET") {
		t.Errorf("Load() error = %v, want it to mention JWT_SECRET", err)
	}
}
