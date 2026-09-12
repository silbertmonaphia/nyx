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

// TestLoad_AcceptsLLMDisabled_ByDefault is the common path: no LLM_*
// env vars set, LLM_ENABLED defaults to false, Load returns a
// valid Config with the LLM zero values. Without this baseline the
// LLM-enabled tests would mask a regression where LLM_ENABLED
// defaults to true.
func TestLoad_AcceptsLLMDisabled_ByDefault(t *testing.T) {
	t.Setenv("DB_URL", "postgres://x")
	t.Setenv("JWT_SECRET", auth.TestSecret)
	t.Setenv("LLM_ENABLED", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil (LLM off by default)", err)
	}
	if cfg.LLMEnabled {
		t.Error("LLMEnabled default = true, want false")
	}
}

// TestLoad_RejectsLLMEnabledWithoutBaseURL pins the fail-closed
// contract: turning LLM on without a base URL is operator
// misconfiguration; the backend must refuse to start so a missing
// env var doesn't silently default to OpenAI's hosted endpoint.
func TestLoad_RejectsLLMEnabledWithoutBaseURL(t *testing.T) {
	t.Setenv("DB_URL", "postgres://x")
	t.Setenv("JWT_SECRET", auth.TestSecret)
	t.Setenv("LLM_ENABLED", "true")
	t.Setenv("LLM_BASE_URL", "")
	t.Setenv("LLM_API_KEY", "sk-prod-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	t.Setenv("LLM_MODEL", "gpt-4o-mini")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want LLM_BASE_URL-required rejection")
	}
	if !strings.Contains(err.Error(), "LLM_BASE_URL") {
		t.Errorf("Load() error = %v, want it to mention LLM_BASE_URL", err)
	}
}

// TestLoad_RejectsLLMEnabledWithPlaceholderKey catches the
// "I committed my .env with 'your-key'" foot-gun. The placeholder
// list is intentionally narrow — it only blocks the patterns a
// careless operator is likely to leave in, not a determined
// attacker (who would just type a real-looking key).
func TestLoad_RejectsLLMEnabledWithPlaceholderKey(t *testing.T) {
	t.Setenv("DB_URL", "postgres://x")
	t.Setenv("JWT_SECRET", auth.TestSecret)
	t.Setenv("LLM_ENABLED", "true")
	t.Setenv("LLM_BASE_URL", "https://api.openai.com/v1")
	t.Setenv("LLM_API_KEY", "your-key-here-replace-me")
	t.Setenv("LLM_MODEL", "gpt-4o-mini")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want placeholder-LLM_API_KEY rejection")
	}
	if !strings.Contains(err.Error(), "LLM_API_KEY") {
		t.Errorf("Load() error = %v, want it to mention LLM_API_KEY", err)
	}
}

// TestLoad_RejectsLLMEnabledWithoutModel asserts the third
// cross-field requirement. Sk-anything-without-a-model would
// otherwise default to a 404 on every call.
func TestLoad_RejectsLLMEnabledWithoutModel(t *testing.T) {
	t.Setenv("DB_URL", "postgres://x")
	t.Setenv("JWT_SECRET", auth.TestSecret)
	t.Setenv("LLM_ENABLED", "true")
	t.Setenv("LLM_BASE_URL", "https://api.openai.com/v1")
	t.Setenv("LLM_API_KEY", "sk-prod-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	t.Setenv("LLM_MODEL", "")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want LLM_MODEL-required rejection")
	}
	if !strings.Contains(err.Error(), "LLM_MODEL") {
		t.Errorf("Load() error = %v, want it to mention LLM_MODEL", err)
	}
}

// TestLoad_AcceptsValidLLMConfig pins the happy path: every
// required field set, durations valid, system prompt present.
// Without this the failure-mode tests above could mask a regression
// where every LLM_ENABLED=true case errors.
func TestLoad_AcceptsValidLLMConfig(t *testing.T) {
	t.Setenv("DB_URL", "postgres://x")
	t.Setenv("JWT_SECRET", auth.TestSecret)
	t.Setenv("LLM_ENABLED", "true")
	t.Setenv("LLM_BASE_URL", "https://api.openai.com/v1")
	t.Setenv("LLM_API_KEY", "sk-prod-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	t.Setenv("LLM_MODEL", "gpt-4o-mini")
	t.Setenv("LLM_TIMEOUT", "30s")
	t.Setenv("LLM_MAX_STREAM_DURATION", "5m")
	t.Setenv("LLM_SYSTEM_PROMPT", "You are a helpful movie catalog assistant.")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil for valid LLM config", err)
	}
	if !cfg.LLMEnabled {
		t.Error("LLMEnabled = false, want true")
	}
	if cfg.LLMProvider != "openai" {
		t.Errorf("LLMProvider default = %q, want %q", cfg.LLMProvider, llmProviderOpenAI)
	}
	if cfg.LLMAllowPrivateURL {
		t.Error("LLMAllowPrivateURL default = true, want false")
	}
	if cfg.LLMMaxTokens != 1024 {
		t.Errorf("LLMMaxTokens default = %d, want 1024", cfg.LLMMaxTokens)
	}
	if cfg.LLMMaxHistoryMessages != 50 {
		t.Errorf("LLMMaxHistoryMessages default = %d, want 50", cfg.LLMMaxHistoryMessages)
	}
}

// TestLoad_RejectsInvalidLLMProvider pins the LLM_PROVIDER
// allowlist — anything outside {openai, vllm} must refuse to
// start so a typo doesn't silently fall back to OpenAI.
func TestLoad_RejectsInvalidLLMProvider(t *testing.T) {
	t.Setenv("DB_URL", "postgres://x")
	t.Setenv("JWT_SECRET", auth.TestSecret)
	t.Setenv("LLM_ENABLED", "true")
	t.Setenv("LLM_PROVIDER", "anthropic")
	t.Setenv("LLM_BASE_URL", "https://api.openai.com/v1")
	t.Setenv("LLM_API_KEY", "sk-prod-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	t.Setenv("LLM_MODEL", "gpt-4o-mini")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want LLM_PROVIDER rejection")
	}
	if !strings.Contains(err.Error(), "LLM_PROVIDER") {
		t.Errorf("Load() error = %v, want it to mention LLM_PROVIDER", err)
	}
}

// TestLoad_AcceptsVLLMProviderWithEmptyAPIKey pins the vLLM happy
// path: LLM_PROVIDER=vllm allows an empty LLM_API_KEY (vLLM
// started without --api-key accepts any Authorization header).
// The OpenAI provider would reject the same configuration.
func TestLoad_AcceptsVLLMProviderWithEmptyAPIKey(t *testing.T) {
	t.Setenv("DB_URL", "postgres://x")
	t.Setenv("JWT_SECRET", auth.TestSecret)
	t.Setenv("LLM_ENABLED", "true")
	t.Setenv("LLM_PROVIDER", "vllm")
	t.Setenv("LLM_BASE_URL", "http://vllm:8000/v1")
	t.Setenv("LLM_API_KEY", "")
	t.Setenv("LLM_MODEL", "Qwen/Qwen2.5-3B-Instruct")
	t.Setenv("LLM_ALLOW_PRIVATE_URL", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil for valid vLLM config", err)
	}
	if cfg.LLMProvider != "vllm" {
		t.Errorf("LLMProvider = %q, want vllm", cfg.LLMProvider)
	}
}

// TestLoad_RejectsVLLMWithInvalidBaseURLScheme covers the SSRF
// scheme allowlist.
func TestLoad_RejectsVLLMWithInvalidBaseURLScheme(t *testing.T) {
	t.Setenv("DB_URL", "postgres://x")
	t.Setenv("JWT_SECRET", auth.TestSecret)
	t.Setenv("LLM_ENABLED", "true")
	t.Setenv("LLM_PROVIDER", "vllm")
	t.Setenv("LLM_BASE_URL", "file:///etc/passwd")
	t.Setenv("LLM_API_KEY", "")
	t.Setenv("LLM_MODEL", "test")
	t.Setenv("LLM_ALLOW_PRIVATE_URL", "true")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want scheme rejection")
	}
	if !strings.Contains(err.Error(), "scheme") {
		t.Errorf("Load() error = %v, want it to mention scheme", err)
	}
}

// TestLoad_RejectsVLLMWithPrivateHost confirms the IP-class
// allowlist at startup.
func TestLoad_RejectsVLLMWithPrivateHost(t *testing.T) {
	t.Setenv("DB_URL", "postgres://x")
	t.Setenv("JWT_SECRET", auth.TestSecret)
	t.Setenv("LLM_ENABLED", "true")
	t.Setenv("LLM_PROVIDER", "vllm")
	t.Setenv("LLM_BASE_URL", "http://127.0.0.1:8000/v1")
	t.Setenv("LLM_API_KEY", "")
	t.Setenv("LLM_MODEL", "test")
	// LLM_ALLOW_PRIVATE_URL intentionally unset.

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want private-host rejection")
	}
	if !strings.Contains(err.Error(), "LLM_ALLOW_PRIVATE_URL") {
		t.Errorf("Load() error = %v, want it to mention the escape hatch", err)
	}
}

// TestLoad_RejectsVLLMWithUserinfo confirms URLs with embedded
// userinfo are refused.
func TestLoad_RejectsVLLMWithUserinfo(t *testing.T) {
	t.Setenv("DB_URL", "postgres://x")
	t.Setenv("JWT_SECRET", auth.TestSecret)
	t.Setenv("LLM_ENABLED", "true")
	t.Setenv("LLM_PROVIDER", "vllm")
	t.Setenv("LLM_BASE_URL", "http://attacker:pw@vllm.example.com/v1")
	t.Setenv("LLM_API_KEY", "")
	t.Setenv("LLM_MODEL", "test")
	t.Setenv("LLM_ALLOW_PRIVATE_URL", "true")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want userinfo rejection")
	}
	if !strings.Contains(err.Error(), "userinfo") {
		t.Errorf("Load() error = %v, want it to mention userinfo", err)
	}
}

// TestLoad_RejectsOpenAIWithoutAPIKey pins that the OpenAI provider
// still requires a non-empty LLM_API_KEY — only vLLM is allowed to
// run keyless.
func TestLoad_RejectsOpenAIWithoutAPIKey(t *testing.T) {
	t.Setenv("DB_URL", "postgres://x")
	t.Setenv("JWT_SECRET", auth.TestSecret)
	t.Setenv("LLM_ENABLED", "true")
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_BASE_URL", "https://api.openai.com/v1")
	t.Setenv("LLM_API_KEY", "")
	t.Setenv("LLM_MODEL", "gpt-4o-mini")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want OpenAI-without-API-key rejection")
	}
	if !strings.Contains(err.Error(), "LLM_API_KEY") {
		t.Errorf("Load() error = %v, want it to mention LLM_API_KEY", err)
	}
}

// TestLoad_RejectsInvalidLLMTimeout pins duration parsing for
// LLM_TIMEOUT. A typo in the env file would otherwise crash on
// the first chat request; better to fail at startup.
func TestLoad_RejectsInvalidLLMTimeout(t *testing.T) {
	t.Setenv("DB_URL", "postgres://x")
	t.Setenv("JWT_SECRET", auth.TestSecret)
	t.Setenv("LLM_ENABLED", "true")
	t.Setenv("LLM_BASE_URL", "https://api.openai.com/v1")
	t.Setenv("LLM_API_KEY", "sk-prod-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	t.Setenv("LLM_MODEL", "gpt-4o-mini")
	t.Setenv("LLM_TIMEOUT", "not-a-duration")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want LLM_TIMEOUT parse error")
	}
	if !strings.Contains(err.Error(), "LLM_TIMEOUT") {
		t.Errorf("Load() error = %v, want it to mention LLM_TIMEOUT", err)
	}
}
