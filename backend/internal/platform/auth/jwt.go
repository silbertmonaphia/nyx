package auth

import (
	"errors"
	"fmt"
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
func NewTokenService(secret []byte, accessTTL time.Duration) (TokenService, error) {
	if IsDefault(string(secret)) || len(secret) < MinSecretBytes {
		return nil, fmt.Errorf("auth: refusing insecure JWT secret (len=%d)", len(secret))
	}
	if accessTTL <= 0 {
		return nil, fmt.Errorf("auth: accessTTL must be positive, got %v", accessTTL)
	}
	cp := make([]byte, len(secret))
	copy(cp, secret)
	return &jwtService{secret: cp, accessTTL: accessTTL}, nil
}

type jwtService struct {
	secret    []byte
	accessTTL time.Duration
}

func (s *jwtService) GenerateToken(userID int, username string) (string, error) {
	claims := Claims{
		UserID:   userID,
		Username: username,
		Type:     "access",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(s.accessTTL)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(s.secret)
}

func (s *jwtService) ValidateToken(tokenString string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		return s.secret, nil
	})

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
