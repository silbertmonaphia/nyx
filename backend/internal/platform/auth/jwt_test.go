package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// newTestTokenService builds a per-test TokenService. The service
// captures its own copy of the secret, so no cleanup is required —
// nothing process-wide is mutated.
func newTestTokenService(t *testing.T) TokenService {
	t.Helper()
	tokens, err := NewTokenService([]byte(TestSecret))
	if err != nil {
		t.Fatalf("NewTokenService: %v", err)
	}
	return tokens
}

func TestIsDefault(t *testing.T) {
	if !IsDefault("your-default-secret-key-change-it-in-prod") {
		t.Error("IsDefault should return true for the development placeholder")
	}
	if IsDefault("some-other-secret") {
		t.Error("IsDefault should return false for other strings")
	}
}

func TestMinSecretBytesIs32(t *testing.T) {
	if MinSecretBytes != 32 {
		t.Errorf("MinSecretBytes = %d, want 32", MinSecretBytes)
	}
}

func TestNewTokenService_RejectsDefaultAndShort(t *testing.T) {
	// defaultSecret is now unexported. Use the literal directly — it's a build-time constant.
	if _, err := NewTokenService([]byte("your-default-secret-key-change-it-in-prod")); err == nil {
		t.Error("default placeholder should be rejected")
	}
	if _, err := NewTokenService([]byte("short")); err == nil {
		t.Error("short key should be rejected")
	}
	if _, err := NewTokenService([]byte(TestSecret)); err != nil {
		t.Errorf("32-byte key rejected: %v", err)
	}
}

func TestRoundTrip_Claims(t *testing.T) {
	tokens := newTestTokenService(t)

	tok, err := tokens.GenerateToken(42, "alice")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if tok == "" {
		t.Fatal("GenerateToken returned empty string")
	}

	claims, err := tokens.ValidateToken(tok)
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
	tokens := newTestTokenService(t)

	tok, err := tokens.GenerateToken(1, "alice")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	// Flip a byte in the signature segment (the third dot-separated
	// part). Any change should invalidate the HMAC.
	tampered := tok[:len(tok)-2] + "AA"

	_, err = tokens.ValidateToken(tampered)
	if !errors.Is(err, ErrInvalidToken) {
		t.Errorf("tampered token: err = %v, want ErrInvalidToken", err)
	}
}

// TestValidateToken_ExpiredToken returns ErrExpiredToken when the
// token's ExpiresAt is in the past. This is the path that lets the
// middleware distinguish "old token, ask user to re-login" (401) from
// "bad token, possibly attacker" (401 + log).
func TestValidateToken_ExpiredToken(t *testing.T) {
	tokens := newTestTokenService(t)

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
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(TestSecret))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	_, err = tokens.ValidateToken(tok)
	if !errors.Is(err, ErrExpiredToken) {
		t.Errorf("expired token: err = %v, want ErrExpiredToken", err)
	}
}

// TestValidateToken_WrongSecret rejects a token signed with a
// different secret. Combined with TestValidateToken_TamperedSignature
// this pins the "only the issuer's secret works" property.
func TestValidateToken_WrongSecret(t *testing.T) {
	tokens := newTestTokenService(t)

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

	_, err = tokens.ValidateToken(tok)
	if !errors.Is(err, ErrInvalidToken) {
		t.Errorf("wrong-secret token: err = %v, want ErrInvalidToken", err)
	}
}

// TestValidateToken_Malformed confirms the parser returns
// ErrInvalidToken (not a panic) for a garbage input string.
func TestValidateToken_Malformed(t *testing.T) {
	tokens := newTestTokenService(t)
	_, err := tokens.ValidateToken("not-a-jwt")
	if !errors.Is(err, ErrInvalidToken) {
		t.Errorf("malformed token: err = %v, want ErrInvalidToken", err)
	}
}
