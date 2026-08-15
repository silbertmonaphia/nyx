package user

import (
	"context"
	"errors"
	"time"

	"nyx/internal/platform/auth"

	"golang.org/x/crypto/bcrypt"
)

var (
	ErrInvalidCredentials = errors.New("invalid username or password")

	// ErrInvalidRefreshToken — the refresh token either doesn't exist
	// in the DB or its hash doesn't match a row. Distinguished from
	// ErrRefreshTokenExpired (which is a real, formerly-valid token
	// that ran past expires_at) so callers can decide whether to
	// surface a generic auth error vs. trigger a re-login flow.
	ErrInvalidRefreshToken = errors.New("invalid refresh token")

	// ErrRefreshTokenReuse — the token exists but has already been
	// revoked (typically because it was previously rotated). The
	// service layer revokes the entire family on this path because
	// RFC 9700 / OAuth 2.0 BCP treat reuse of a rotated refresh token
	// as evidence of theft.
	ErrRefreshTokenReuse = errors.New("refresh token reuse detected")

	// ErrRefreshTokenExpired — the token's expires_at is in the past.
	// The family is NOT revoked on this path: the user simply waits
	// too long and logs in again.
	ErrRefreshTokenExpired = errors.New("refresh token expired")
)

type Service interface {
	Register(ctx context.Context, req RegisterRequest) (*AuthResponse, error)
	Login(ctx context.Context, req LoginRequest) (*AuthResponse, error)
	Refresh(ctx context.Context, req RefreshRequest) (*AuthResponse, error)
	Logout(ctx context.Context, req LogoutRequest) error
}

// service is the user/auth domain service. tokens issues access JWTs;
// accessTTL / refreshTTL are captured here so the service can mint
// tokens with the configured lifetimes and stamp refresh tokens with
// the right expiry.
type service struct {
	repo       Repository
	tokens     auth.TokenService
	accessTTL  time.Duration
	refreshTTL time.Duration
}

// NewService builds the user/auth service. The two TTLs feed both
// access-token minting (accessTTL is forwarded into tokens) and
// refresh-token row expiry (refreshTTL is stamped onto the row when
// CreateRefreshToken runs). They MUST be positive; the cmd/api wiring
// parses them once at startup and refuses to boot on a zero value.
func NewService(repo Repository, tokens auth.TokenService, accessTTL, refreshTTL time.Duration) Service {
	return &service{
		repo:       repo,
		tokens:     tokens,
		accessTTL:  accessTTL,
		refreshTTL: refreshTTL,
	}
}

// mintRefreshToken is a small helper: generate raw bytes, hash them,
// persist the row, return the raw bytes for the response. Lives here
// so Register, Login, and Refresh all build the row identically.
func (s *service) mintRefreshToken(ctx context.Context, userID int) (string, error) {
	raw, hash, err := newRefreshToken()
	if err != nil {
		return "", err
	}
	expires := time.Now().Add(s.refreshTTL)
	if _, err := s.repo.CreateRefreshToken(ctx, userID, hash, expires); err != nil {
		return "", err
	}
	return raw, nil
}

// accessExpires is the wall-clock time the freshly-minted access JWT
// will expire. It is stamped on AuthResponse so clients can
// pre-emptively refresh before a 401 round-trip (the WWW-Authenticate
// header is the reactive trigger; this is the proactive one).
func (s *service) accessExpires() time.Time {
	return time.Now().Add(s.accessTTL)
}

func (s *service) Register(ctx context.Context, req RegisterRequest) (*AuthResponse, error) {
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}

	u := &User{
		Username:     req.Username,
		Email:        req.Email,
		PasswordHash: string(hashedPassword),
	}

	if err := s.repo.CreateUser(ctx, u); err != nil {
		return nil, err
	}

	token, err := s.tokens.GenerateToken(u.ID, u.Username)
	if err != nil {
		return nil, err
	}

	refresh, err := s.mintRefreshToken(ctx, u.ID)
	if err != nil {
		return nil, err
	}

	return &AuthResponse{
		Token:        token,
		RefreshToken: refresh,
		ExpiresAt:    s.accessExpires(),
		User:         *u,
	}, nil
}

func (s *service) Login(ctx context.Context, req LoginRequest) (*AuthResponse, error) {
	u, err := s.repo.GetUserByUsername(ctx, req.Username)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}

	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(req.Password)); err != nil {
		return nil, ErrInvalidCredentials
	}

	token, err := s.tokens.GenerateToken(u.ID, u.Username)
	if err != nil {
		return nil, err
	}

	refresh, err := s.mintRefreshToken(ctx, u.ID)
	if err != nil {
		return nil, err
	}

	return &AuthResponse{
		Token:        token,
		RefreshToken: refresh,
		ExpiresAt:    s.accessExpires(),
		User:         *u,
	}, nil
}

// Refresh validates the supplied refresh token, rotates it (issuing a
// new pair), and returns the new AuthResponse. Three failure modes:
//
//  1. Token doesn't exist / hash mismatch -> ErrInvalidRefreshToken
//  2. Token has revoked_at != nil -> family revocation +
//     ErrRefreshTokenReuse (the smoking gun for stolen-token replay)
//  3. Token past expires_at -> ErrRefreshTokenExpired (no family revoke;
//     legitimate timeout, not a theft signal)
//
// The atomic CTE in RotateRefreshToken handles concurrent rotations:
// the second caller observes the new revoked_at on the old row and
// takes the reuse branch above.
func (s *service) Refresh(ctx context.Context, req RefreshRequest) (*AuthResponse, error) {
	suppliedHash := sha256Sum(req.RefreshToken)

	row, err := s.repo.GetRefreshTokenByHash(ctx, suppliedHash)
	if err != nil {
		if errors.Is(err, ErrRefreshTokenNotFound) {
			return nil, ErrInvalidRefreshToken
		}
		return nil, err
	}

	// Reuse detection: a row with revoked_at != nil means this token
	// was already rotated. Per RFC 9700 we revoke the entire family
	// (could be a stolen token being replayed) and reject.
	if row.RevokedAt != nil {
		// Best-effort family revoke — don't shadow the primary error.
		_, _ = s.repo.RevokeRefreshTokenFamily(ctx, row.FamilyID)
		return nil, ErrRefreshTokenReuse
	}

	if time.Now().After(row.ExpiresAt) {
		return nil, ErrRefreshTokenExpired
	}

	// Happy path: rotate. Mint a new raw token, hash it, atomic CTE
	// stamps the old row revoked + links it via replaced_by_id.
	newRaw, newHash, err := newRefreshToken()
	if err != nil {
		return nil, err
	}
	newExpires := time.Now().Add(s.refreshTTL)
	newRow, err := s.repo.RotateRefreshToken(ctx, row.ID, row.UserID, newHash, row.FamilyID, newExpires)
	if err != nil {
		return nil, err
	}
	_ = newRow // Row already projected; we only need the raw for the response.

	// Look up the user so the response carries the up-to-date record
	// (matches the Register/Login paths).
	user, err := s.repo.GetUserByID(ctx, row.UserID)
	if err != nil {
		return nil, err
	}

	accessToken, err := s.tokens.GenerateToken(user.ID, user.Username)
	if err != nil {
		return nil, err
	}

	return &AuthResponse{
		Token:        accessToken,
		RefreshToken: newRaw,
		ExpiresAt:    s.accessExpires(),
		User:         *user,
	}, nil
}

// Logout revokes the entire refresh-token family the supplied token
// belongs to. Idempotent: an unknown token returns nil rather than
// leaking that the token doesn't exist. Per-design v1 behavior: a
// single Logout kills every active session for that user (logout on
// phone also logs out the laptop) — per-device logout is a deliberate
// follow-up tracked in FUTURE.md.
func (s *service) Logout(ctx context.Context, req LogoutRequest) error {
	suppliedHash := sha256Sum(req.RefreshToken)
	row, err := s.repo.GetRefreshTokenByHash(ctx, suppliedHash)
	if err != nil {
		if errors.Is(err, ErrRefreshTokenNotFound) {
			return nil // idempotent
		}
		return err
	}
	_, err = s.repo.RevokeRefreshTokenFamily(ctx, row.FamilyID)
	return err
}
