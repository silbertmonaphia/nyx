package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// newTestTokenService builds a per-test TokenService with a 15-minute
// access TTL (mirrors JWT_ACCESS_TTL's production default). The
// service captures its own copy of the secret, so no cleanup is
// required — nothing process-wide is mutated.
func newTestTokenService(t *testing.T) TokenService {
	t.Helper()
	tokens, err := NewTokenService([]byte(TestSecret), 15*time.Minute)
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
	if _, err := NewTokenService([]byte("your-default-secret-key-change-it-in-prod"), 15*time.Minute); err == nil {
		t.Error("default placeholder should be rejected")
	}
	if _, err := NewTokenService([]byte("short"), 15*time.Minute); err == nil {
		t.Error("short key should be rejected")
	}
	if _, err := NewTokenService([]byte(TestSecret), 0); err == nil {
		t.Error("zero access TTL should be rejected")
	}
	if _, err := NewTokenService([]byte(TestSecret), -time.Minute); err == nil {
		t.Error("negative access TTL should be rejected")
	}
	if _, err := NewTokenService([]byte(TestSecret), 15*time.Minute); err != nil {
		t.Errorf("32-byte key with positive TTL rejected: %v", err)
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
		Type:     "access",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    Issuer,
			Audience:  jwt.ClaimStrings{Audience},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-1 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now().Add(-2 * time.Hour)),
			NotBefore: jwt.NewNumericDate(time.Now().Add(-2 * time.Hour)),
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
		Type:     "access",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    Issuer,
			Audience:  jwt.ClaimStrings{Audience},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			NotBefore: jwt.NewNumericDate(time.Now()),
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

// TestJWT_TypeClaimRequired is the refresh-token defense-in-depth:
// a JWT minted with Type != "access" must be rejected by
// ValidateToken, even when the signature is otherwise valid. Refresh
// tokens are opaque and never minted as JWTs today, but this guard
// stops a future code path that accidentally hands out a refresh
// JWT from being usable as a Bearer credential.
func TestJWT_TypeClaimRequired(t *testing.T) {
	// Sign a token with Type: "refresh" directly, bypassing the
	// TokenService's GenerateToken (which always sets Type: "access").
	claims := Claims{
		UserID:   1,
		Username: "alice",
		Type:     "refresh",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    Issuer,
			Audience:  jwt.ClaimStrings{Audience},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			NotBefore: jwt.NewNumericDate(time.Now()),
		},
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(TestSecret))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	tokens := newTestTokenService(t)
	_, err = tokens.ValidateToken(tok)
	if !errors.Is(err, ErrInvalidToken) {
		t.Errorf("refresh-typed token: err = %v, want ErrInvalidToken", err)
	}
}

// TestJWT_GenerateSetsAccessTTL pins the configured TTL on
// GenerateToken's output. We allow a small skew to absorb the
// sub-second difference between time.Now() in GenerateToken and the
// test's readback; the assertion only needs to confirm the TTL is
// applied (not the hard-coded 24h).
func TestJWT_GenerateSetsAccessTTL(t *testing.T) {
	tokens, err := NewTokenService([]byte(TestSecret), 15*time.Minute)
	if err != nil {
		t.Fatalf("NewTokenService: %v", err)
	}
	tok, err := tokens.GenerateToken(1, "alice")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	claims, err := tokens.ValidateToken(tok)
	if err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}
	if claims.Type != "access" {
		t.Errorf("claims.Type = %q, want \"access\"", claims.Type)
	}
	if claims.ExpiresAt == nil {
		t.Fatal("claims.ExpiresAt is nil")
	}
	// 15m ± 30s skew (test runtime between generate and validate).
	want := 15 * time.Minute
	got := time.Until(claims.ExpiresAt.Time)
	diff := got - want
	if diff < -30*time.Second || diff > 30*time.Second {
		t.Errorf("ExpiresAt in %v, want %v (±30s)", got, want)
	}
}

// TestJWT_StampsIssuerAudience pins the iss/aud claims that H3/H4
// require. Without these stamps the parser would reject every token
// minted by GenerateToken, so this test guards the round-trip.
func TestJWT_StampsIssuerAudience(t *testing.T) {
	tokens := newTestTokenService(t)
	tok, err := tokens.GenerateToken(1, "alice")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	claims, err := tokens.ValidateToken(tok)
	if err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}
	if claims.Issuer != Issuer {
		t.Errorf("claims.Issuer = %q, want %q", claims.Issuer, Issuer)
	}
	if len(claims.Audience) != 1 || claims.Audience[0] != Audience {
		t.Errorf("claims.Audience = %v, want [%q]", claims.Audience, Audience)
	}
}

// TestJWT_RejectsForeignIssuer confirms WithIssuer enforces the
// expected issuer value. A token minted with a different iss must
// fail validation even when the signature is otherwise valid.
func TestJWT_RejectsForeignIssuer(t *testing.T) {
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		UserID:   1,
		Username: "alice",
		Type:     "access",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "evil-issuer",
			Audience:  jwt.ClaimStrings{Audience},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			NotBefore: jwt.NewNumericDate(time.Now()),
		},
	}).SignedString([]byte(TestSecret))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	tokens := newTestTokenService(t)
	if _, err := tokens.ValidateToken(tok); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("foreign-issuer token: err = %v, want ErrInvalidToken", err)
	}
}

// TestJWT_RejectsForeignAudience confirms WithAudience enforces the
// expected audience value. Cross-API replay of an Nyx token against
// a different backend should fail.
func TestJWT_RejectsForeignAudience(t *testing.T) {
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		UserID:   1,
		Username: "alice",
		Type:     "access",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    Issuer,
			Audience:  jwt.ClaimStrings{"some-other-api"},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			NotBefore: jwt.NewNumericDate(time.Now()),
		},
	}).SignedString([]byte(TestSecret))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	tokens := newTestTokenService(t)
	if _, err := tokens.ValidateToken(tok); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("foreign-audience token: err = %v, want ErrInvalidToken", err)
	}
}

// TestJWT_RejectsUnexpectedAlg confirms WithValidMethods refuses a
// token whose alg is anything other than HS256. This is the defense
// against the alg=none / RS256→HMAC confusion attack: the keyfunc
// should never even be called for a non-HMAC alg.
func TestJWT_RejectsUnexpectedAlg(t *testing.T) {
	// jwt.SigningMethodNone requires a sentinel key. The signing
	// itself succeeds; what matters is that our validator rejects
	// the resulting token.
	tok, err := jwt.NewWithClaims(jwt.SigningMethodNone, Claims{
		UserID:   1,
		Username: "alice",
		Type:     "access",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    Issuer,
			Audience:  jwt.ClaimStrings{Audience},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			NotBefore: jwt.NewNumericDate(time.Now()),
		},
	}).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	tokens := newTestTokenService(t)
	if _, err := tokens.ValidateToken(tok); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("alg=none token: err = %v, want ErrInvalidToken", err)
	}
}

// TestNewTokenService_ErrorHidesSecretLength pins M4: the error
// string returned for a short secret must not include the supplied
// length. A caller holding only the error message should not be able
// to learn how close they were to MinSecretBytes.
func TestNewTokenService_ErrorHidesSecretLength(t *testing.T) {
	_, err := NewTokenService([]byte("short"), 15*time.Minute)
	if err == nil {
		t.Fatal("expected error for short secret")
	}
	if strings.Contains(err.Error(), "len=") || strings.Contains(err.Error(), "short=5") {
		t.Errorf("error leaks secret length: %q", err.Error())
	}
}
