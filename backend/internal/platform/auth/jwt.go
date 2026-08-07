package auth

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var (
	ErrInvalidToken = errors.New("invalid token")
	ErrExpiredToken = errors.New("token expired")
)

// DefaultSecret is the development placeholder used when no secret is
// configured. It mirrors the JWT_SECRET default in
// internal/platform/config and must be overridden in production.
const DefaultSecret = "your-default-secret-key-change-it-in-prod"

// secret is the HMAC signing key used by GenerateToken and
// ValidateToken. It is process-wide state, set once during startup by
// SetSecret (from cfg.JWTSecret) before the HTTP server begins serving.
// Reads afterwards are never concurrent with a write, so no lock is
// needed.
var secret = []byte(DefaultSecret)

// SetSecret configures the JWT signing key for the process. Call it
// once from main after config.Load and before serving traffic. An empty
// string keeps DefaultSecret, matching viper's default so a missing
// JWT_SECRET behaves the same as it always has: tokens still sign, and
// production is expected to override the placeholder.
func SetSecret(s string) {
	if s == "" {
		secret = []byte(DefaultSecret)
		return
	}
	secret = []byte(s)
}

type Claims struct {
	UserID   int    `json:"user_id"`
	Username string `json:"username"`
	jwt.RegisteredClaims
}

func GenerateToken(userID int, username string) (string, error) {
	claims := Claims{
		UserID:   userID,
		Username: username,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(secret)
}

func ValidateToken(tokenString string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		return secret, nil
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

	return claims, nil
}
