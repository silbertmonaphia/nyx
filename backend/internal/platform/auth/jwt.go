package auth

import (
	"errors"
	"fmt"
	"runtime"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var (
	ErrInvalidToken = errors.New("invalid token")
	ErrExpiredToken = errors.New("token expired")
)

// defaultSecret is the development placeholder used when no secret is
// configured. It mirrors the JWT_SECRET default in
// internal/platform/config and must be overridden in production.
// Unexported — callers should rely on IsDefault for comparisons.
const defaultSecret = "your-default-secret-key-change-it-in-prod"

// MinSecretBytes is the minimum key length accepted for HS256 JWTs.
// RFC 7518 §3.2 mandates >= 256 bits.
const MinSecretBytes = 32

// TestSecret is a fixed 32-byte secret used by tests across the
// codebase. Importing packages reference it instead of inlining the
// literal — keeps the test fixtures in sync if the minimum length or
// the secret format ever changes.
const TestSecret = "01234567890123456789012345678901"

// Issuer is the value stamped in the `iss` claim on every token the
// service mints and the only value the parser accepts. Hard-coded
// because Nyx runs a single auth surface today; if a future
// deployment ever fans out to multiple APIs, lift this into config
// alongside JWT_SECRET.
//
// Audience is the recipient the token is intended for. Same single-
// deployment rationale as Issuer.
const (
	Issuer   = "nyx-auth"
	Audience = "nyx-api"
)

// IsDefault reports whether s equals the built-in development
// placeholder. Used by config validation to refuse insecure secrets.
func IsDefault(s string) bool {
	return s == defaultSecret
}

// Claims is the JWT payload. The Type field distinguishes access
// tokens from refresh tokens; ValidateToken refuses any token whose
// type isn't "access", defending against a refresh token being
// presented at an API endpoint as a Bearer credential.
type Claims struct {
	UserID   int    `json:"user_id"`
	Username string `json:"username"`
	Type     string `json:"type,omitempty"`
	jwt.RegisteredClaims
}

// TokenService issues and validates JWTs. The signing key is captured
// at construction; there is no shared mutable state.
type TokenService interface {
	GenerateToken(userID int, username string) (string, error)
	ValidateToken(tokenString string) (*Claims, error)
}

// NewTokenService validates secret and returns a TokenService. It
// refuses the built-in default and any key shorter than MinSecretBytes
// so misconfiguration fails closed at startup, not silently in prod.
// accessTTL is captured here (not on the service caller) so every
// code path that mints a token via the service gets the configured
// lifetime — there's no way for the caller to forget.
//
// The secret is defensively copied so callers can zero their own
// buffers after construction. A finalizer schedules best-effort
// zeroisation when the service becomes unreachable (compliance with
// NIST SP 800-57 §5.3.6; the heap-clearing guarantee is best-effort
// because the GC is free to drop the finalizer if the object stays
// referenced).
func NewTokenService(secret []byte, accessTTL time.Duration) (TokenService, error) {
	if IsDefault(string(secret)) || len(secret) < MinSecretBytes {
		// Error string is intentionally free of `len=` to avoid
		// confirming the secret length to a caller holding only the
		// error message (see SECURITY.md M4).
		return nil, fmt.Errorf("auth: refusing insecure JWT secret")
	}
	if accessTTL <= 0 {
		return nil, fmt.Errorf("auth: accessTTL must be positive, got %v", accessTTL)
	}
	cp := make([]byte, len(secret))
	copy(cp, secret)
	svc := &jwtService{secret: cp, accessTTL: accessTTL}
	// Finalizer captures `svc` (not just the secret slice) so it
	// keeps a strong reference and the GC won't reclaim the service
	// out from under us mid-finalize. Best-effort: the runtime may
	// skip finalization if the object stays referenced forever (the
	// normal process lifetime).
	runtime.SetFinalizer(svc, func(s *jwtService) {
		for i := range s.secret {
			s.secret[i] = 0
		}
	})
	return svc, nil
}

type jwtService struct {
	secret    []byte
	accessTTL time.Duration
}

func (s *jwtService) GenerateToken(userID int, username string) (string, error) {
	now := time.Now()
	claims := Claims{
		UserID:   userID,
		Username: username,
		Type:     "access",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    Issuer,
			Audience:  jwt.ClaimStrings{Audience},
			Subject:   fmt.Sprintf("%d", userID),
			ExpiresAt: jwt.NewNumericDate(now.Add(s.accessTTL)),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(s.secret)
}

func (s *jwtService) ValidateToken(tokenString string) (*Claims, error) {
	parserOpts := []jwt.ParserOption{
		// Pin the algorithm. Without this, a token signed with a
		// different alg (e.g. "none", RS256) could pass a keyfunc
		// that returns the HMAC secret and be accepted by a
		// vulnerable verifier — see CVE-2015-9235-class attacks. The
		// keyfunc below adds a redundant assertion on token.Method.
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		// iss/aud must match the values this service mints.
		jwt.WithIssuer(Issuer),
		jwt.WithAudience(Audience),
		// nbf is already in the parser's default checks; the
		// explicit option here is documentary.
		jwt.WithExpirationRequired(),
	}

	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		// Belt-and-braces: even with WithValidMethods, reassert
		// that the method is HMAC before returning the secret. A
		// future parser option regression can't bypass this.
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return s.secret, nil
	}, parserOpts...)

	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrExpiredToken
		}
		return nil, ErrInvalidToken
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, ErrInvalidToken
	}

	// Type guard: a token presented at an authenticated endpoint
	// must be an access token. Refresh tokens are opaque and never
	// minted as JWTs, but defending here means a future
	// accidentally-signed refresh token can't be used as a Bearer.
	if claims.Type != "access" {
		return nil, ErrInvalidToken
	}

	return claims, nil
}
