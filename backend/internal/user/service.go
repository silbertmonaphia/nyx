package user

import (
	"context"
	"errors"
	"fmt"
	"time"

	"nyx/internal/platform/auth"

	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel/trace"
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

// dummyBcryptHash is a pre-computed bcrypt hash used to equalise the
// Login timing between the "username exists, wrong password" branch
// and the "no such user" branch. Without this an attacker who can
// measure login latency can enumerate registered usernames — bcrypt
// cost dominates the request budget on the success branch and is
// absent on the not-found branch (see SECURITY.md H2).
//
// Cost matches bcrypt.DefaultCost (10); the plaintext is irrelevant
// because we only ever compare against it, never derive it from user
// input. Computed once at process init via init() — the cost is paid
// exactly once across the process lifetime.
var dummyBcryptHash []byte

func init() {
	h, err := bcrypt.GenerateFromPassword([]byte("timing-equaliser-not-a-real-password"), bcrypt.DefaultCost)
	if err != nil {
		// bcrypt init failure is unrecoverable; the package can't
		// function without a dummy hash on the not-found branch.
		panic(fmt.Errorf("user: init: bcrypt dummy hash: %w", err))
	}
	dummyBcryptHash = h
}

type Service interface {
	Register(ctx context.Context, req RegisterRequest) (*AuthResult, error)
	Login(ctx context.Context, req LoginRequest) (*AuthResult, error)
	// Refresh takes the raw refresh token read by the handler from
	// the __Host-nyx-refresh cookie. The body is empty by design —
	// huma never sees a refresh_token field, the wire contract is
	// cookie-only.
	Refresh(ctx context.Context, rawRefresh string) (*AuthResult, error)
	// Logout takes the raw refresh token read by the handler from
	// the __Host-nyx-refresh cookie. Same wire contract as Refresh.
	Logout(ctx context.Context, rawRefresh string) error
}

// AuthResult is the service-layer return value: it carries the
// raw tokens the handler needs to set as cookies, plus the
// JSON-serialisable parts (user + expires_at) that survive in the
// response body. Keeping the tokens out of AuthResponse is the
// security boundary — there is no path by which they leak into the
// JSON envelope.
type AuthResult struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	User         User
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
	tracer     trace.Tracer
}

// NewService builds the user/auth service. The two TTLs feed both
// access-token minting (accessTTL is forwarded into tokens) and
// refresh-token row expiry (refreshTTL is stamped onto the row when
// CreateRefreshToken runs). They MUST be positive; the cmd/api wiring
// parses them once at startup and refuses to boot on a zero value.
// tracer emits one OTel span per public method; pass the noop tracer
// when tracing is disabled — Start becomes free.
func NewService(repo Repository, tokens auth.TokenService, accessTTL, refreshTTL time.Duration, tracer trace.Tracer) Service {
	return &service{
		repo:       repo,
		tokens:     tokens,
		accessTTL:  accessTTL,
		refreshTTL: refreshTTL,
		tracer:     tracer,
	}
}

// mintRefreshToken is a small helper: generate raw bytes, hash them,
// persist the row, return the raw bytes for the response. Lives here
// so Register, Login, and Refresh all build the row identically.
//
// Cap enforcement: before inserting, count the user's active rows.
// If the count is at RefreshCap, revoke the oldest row(s) to make
// room. This bounds the per-user table footprint so a stolen-cookie
// flood or a buggy client that doesn't logout can't accumulate
// forever (see SECURITY.md M2).
func (s *service) mintRefreshToken(ctx context.Context, userID int) (string, error) {
	raw, hash, err := newRefreshToken()
	if err != nil {
		return "", err
	}

	if err := s.enforceRefreshCap(ctx, userID); err != nil {
		return "", err
	}

	expires := time.Now().Add(s.refreshTTL)
	if _, err := s.repo.CreateRefreshToken(ctx, userID, hash, expires); err != nil {
		return "", err
	}
	return raw, nil
}

// enforceRefreshCap revokes the user's oldest active rows until the
// active count is below RefreshCap. Called before every mint so the
// cap holds even under concurrent registrations / logins. Revoke
// failures are intentionally swallowed: a stuck cap is a UX papercut
// (one extra active row), not a security hole, and we'd rather let
// the mint proceed than fail it for an unrelated bookkeeping issue.
func (s *service) enforceRefreshCap(ctx context.Context, userID int) error {
	active, err := s.repo.CountActiveRefreshTokensByUser(ctx, userID)
	if err != nil {
		return err
	}
	if active < int64(RefreshCap) {
		return nil
	}
	surplus := int(active) - RefreshCap + 1 // +1 to make room for the about-to-be-minted row
	oldest, err := s.repo.ListOldestActiveRefreshTokensByUser(ctx, userID, surplus)
	if err != nil {
		return err
	}
	for _, id := range oldest {
		if err := s.repo.RevokeRefreshTokenByID(ctx, id); err != nil {
			log.Warn().Err(err).Int64("token_id", id).Msg("revoke during cap enforcement failed; continuing")
		}
	}
	return nil
}

// accessExpires is the wall-clock time the freshly-minted access JWT
// will expire. It is stamped on AuthResponse so clients can
// pre-emptively refresh before a 401 round-trip (the WWW-Authenticate
// header is the reactive trigger; this is the proactive one).
func (s *service) accessExpires() time.Time {
	return time.Now().Add(s.accessTTL)
}

func (s *service) Register(ctx context.Context, req RegisterRequest) (*AuthResult, error) {
	ctx, span := s.tracer.Start(ctx, "user.Register", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()

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
		// Collapse the per-field unique-violation sentinels into a
		// single ErrUserAlreadyExists. Without this, an attacker who
		// probes registration can tell whether a *given* username or
		// a *given* email is already in use (Login only collapses to
		// ErrInvalidCredentials; the two should be symmetric — see
		// SECURITY.md H5).
		if errors.Is(err, ErrUsernameTaken) || errors.Is(err, ErrEmailTaken) {
			return nil, ErrUserAlreadyExists
		}
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

	return &AuthResult{
		AccessToken:  token,
		RefreshToken: refresh,
		ExpiresAt:    s.accessExpires(),
		User:         *u,
	}, nil
}

func (s *service) Login(ctx context.Context, req LoginRequest) (*AuthResult, error) {
	ctx, span := s.tracer.Start(ctx, "user.Login", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()

	u, err := s.repo.GetUserByUsername(ctx, req.Username)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			// Equalise timing with the existing-user branch by
			// running a bcrypt compare against a known-bad hash.
			// Without this, an attacker who can measure login
			// latency can enumerate which usernames are registered
			// (bcrypt cost dominates the request budget on the
			// existing-user branch and is absent here). See
			// SECURITY.md H2.
			_ = bcrypt.CompareHashAndPassword(dummyBcryptHash, []byte(req.Password))
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

	return &AuthResult{
		AccessToken:  token,
		RefreshToken: refresh,
		ExpiresAt:    s.accessExpires(),
		User:         *u,
	}, nil
}

// Refresh validates the supplied refresh token, rotates it (issuing a
// new pair), and returns the new AuthResult. Three failure modes:
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
//
// rawRefresh is read from the __Host-nyx-refresh cookie by the
// handler — it never travels in the request body.
func (s *service) Refresh(ctx context.Context, rawRefresh string) (*AuthResult, error) {
	ctx, span := s.tracer.Start(ctx, "user.Refresh", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()

	if rawRefresh == "" {
		return nil, ErrInvalidRefreshToken
	}

	suppliedHash := sha256Sum(rawRefresh)

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

	return &AuthResult{
		AccessToken:  accessToken,
		RefreshToken: newRaw,
		ExpiresAt:    s.accessExpires(),
		User:         *user,
	}, nil
}

// Logout revokes only the supplied refresh-token row. Idempotent:
// an unknown or empty token returns nil rather than leaking that
// the token doesn't exist. Per-device logout: killing the phone
// session no longer drops the laptop session — each device holds its
// own row in the refresh_tokens table, and revoking the row is
// sufficient to invalidate that session (see SECURITY.md M1).
//
// rawRefresh is read from the __Host-nyx-refresh cookie by the
// handler.
func (s *service) Logout(ctx context.Context, rawRefresh string) error {
	ctx, span := s.tracer.Start(ctx, "user.Logout", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()

	if rawRefresh == "" {
		return nil // idempotent — no token means no work to do
	}

	suppliedHash := sha256Sum(rawRefresh)
	row, err := s.repo.GetRefreshTokenByHash(ctx, suppliedHash)
	if err != nil {
		if errors.Is(err, ErrRefreshTokenNotFound) {
			return nil // idempotent
		}
		return err
	}
	return s.repo.RevokeRefreshTokenByID(ctx, row.ID)
}
