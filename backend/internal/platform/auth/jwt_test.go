package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// TestRoundTrip_Claims verifies GenerateToken + ValidateToken
// preserves the user_id and username claims. This is the smoke
// test every JWT-based auth integration leans on.
func TestRoundTrip_Claims(t *testing.T) {
	// Pin the secret so a CI env override can't change the test
	// outcome. The package reads JWT_SECRET at call time, so the
	// override must be set BEFORE GenerateToken runs.
	t.Setenv("JWT_SECRET", "test-secret-do-not-use-in-prod")

	tok, err := GenerateToken(42, "alice")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if tok == "" {
		t.Fatal("GenerateToken returned empty string")
	}

	claims, err := ValidateToken(tok)
	if err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}
	if claims.UserID != 42 {
		t.Errorf("claims.UserID = %d, want 42", claims.UserID)
	}
	if claims.Username != "alice" {
		t.Errorf("claims.Username = %q, want %q", claims.Username, "alice")
	}
	if claims.ExpiresAt == nil {
		t.Error("claims.ExpiresAt is nil; token has no expiration")
	}
}

// TestValidateToken_TamperedSignature checks that mutating the
// token's signature causes ValidateToken to return ErrInvalidToken.
// Without this guard, an attacker could forge tokens by flipping
// bytes and observing which ones the server accepts.
func TestValidateToken_TamperedSignature(t *testing.T) {
	t.Setenv("JWT_SECRET", "test-secret")

	tok, err := GenerateToken(1, "alice")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	// Flip a byte in the signature segment (the third dot-separated
	// part). Any change should invalidate the HMAC.
	tampered := tok[:len(tok)-2] + "AA"

	_, err = ValidateToken(tampered)
	if !errors.Is(err, ErrInvalidToken) {
		t.Errorf("tampered token: err = %v, want ErrInvalidToken", err)
	}
}

// TestValidateToken_ExpiredToken returns ErrExpiredToken when the
// token's ExpiresAt is in the past. This is the path that lets the
// middleware distinguish "old token, ask user to re-login" (401) from
// "bad token, possibly attacker" (401 + log).
func TestValidateToken_ExpiredToken(t *testing.T) {
	t.Setenv("JWT_SECRET", "test-secret")

	// Mint a token with an ExpiresAt one hour in the past. We
	// construct it directly rather than calling GenerateToken so
	// we can override the expiration.
	claims := Claims{
		UserID:   1,
		Username: "alice",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-1 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now().Add(-2 * time.Hour)),
		},
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("test-secret"))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	_, err = ValidateToken(tok)
	if !errors.Is(err, ErrExpiredToken) {
		t.Errorf("expired token: err = %v, want ErrExpiredToken", err)
	}
}

// TestValidateToken_WrongSecret rejects a token signed with a
// different secret. Combined with TestValidateToken_TamperedSignature
// this pins the "only the issuer's secret works" property.
func TestValidateToken_WrongSecret(t *testing.T) {
	t.Setenv("JWT_SECRET", "test-secret")

	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		UserID:   1,
		Username: "alice",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}).SignedString([]byte("other-secret"))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	_, err = ValidateToken(tok)
	if !errors.Is(err, ErrInvalidToken) {
		t.Errorf("wrong-secret token: err = %v, want ErrInvalidToken", err)
	}
}

// TestValidateToken_Malformed confirms the parser returns
// ErrInvalidToken (not a panic) for a garbage input string.
func TestValidateToken_Malformed(t *testing.T) {
	_, err := ValidateToken("not-a-jwt")
	if !errors.Is(err, ErrInvalidToken) {
		t.Errorf("malformed token: err = %v, want ErrInvalidToken", err)
	}
}

// TestGenerateToken_EmptySecretUsesDefault guards the fallback to
// the placeholder secret when JWT_SECRET is unset. We document this
// in config.go as a development convenience that must be overridden
// in production; the test pins the behavior so an accidental refactor
// doesn't start failing closed (refusing to sign) without anyone
// noticing.
func TestGenerateToken_EmptySecretUsesDefault(t *testing.T) {
	t.Setenv("JWT_SECRET", "")

	tok, err := GenerateToken(1, "alice")
	if err != nil {
		t.Fatalf("GenerateToken with empty secret: %v", err)
	}
	if tok == "" {
		t.Fatal("GenerateToken returned empty string with empty secret")
	}

	// Sanity: validate with the same package default.
	claims, err := ValidateToken(tok)
	if err != nil {
		t.Errorf("ValidateToken of default-secret token: %v", err)
	}
	if claims != nil && claims.UserID != 1 {
		t.Errorf("claims.UserID = %d, want 1", claims.UserID)
	}
}