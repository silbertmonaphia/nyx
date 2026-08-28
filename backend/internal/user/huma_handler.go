package user

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"nyx/internal/middleware"
	"nyx/internal/platform/api"
	"nyx/internal/platform/auth"
	"nyx/internal/reqctx"
)

// Handler exposes user/auth domain operations. It is constructed in
// main.go from a Service and registered onto a huma API via
// RegisterUserOps. No cookie plumbing — auth state rides in the
// request body on login/refresh/logout and in the Authorization
// header on protected routes.
type Handler struct {
	service Service
}

func NewHandler(service Service) *Handler {
	return &Handler{service: service}
}

// RegisterUserOps wires the user endpoints onto a huma API. The
// register/refresh routes are public — no Middlewares — so the
// existing rate-limit cap is the only upstream gate. The /api/logout
// route is auth-required: callers must already hold a valid access
// token in the Authorization header, and the handler revokes the
// refresh-token row identified by the refresh_token in the body.
//
// tokens is threaded through to attach middleware.NewHumaAuth to
// the protected logout and me operations.
func RegisterUserOps(api huma.API, h *Handler, tokens auth.TokenService) {
	RegisterUserOpsTest(api, h, tokens)
}

// RegisterUserOpsTest registers the same operations as RegisterUserOps.
// It exists so handler tests can build a lean chi + huma stack against
// the exact production operation set (mirroring movie's
// RegisterMovieOpsTest). Unlike movie there is no `withAuth` flag:
// register, login, and refresh are public by design, so there is no
// JWT middleware to toggle. Logout is always auth-required.
func RegisterUserOpsTest(api huma.API, h *Handler, tokens auth.TokenService) {
	huma.Register(api, huma.Operation{
		OperationID: "register",
		Method:      http.MethodPost,
		Path:        "/api/register",
		Summary:     "Register a user",
		Description: "Create a new user account. Returns 409 when the username or email is already taken. The response body carries the access token, refresh token, token type, expiry, and user profile — clients store the tokens themselves.",
		Tags:        []string{"auth"},
		// Per-route tighter rate limit so a credential-stuffing or
		// account-enumeration flood can't share the global IP
		// bucket with the rest of the API (see SECURITY.md M3).
		Middlewares: huma.Middlewares{middleware.DefaultAuthRateLimit()},
	}, h.Register)

	huma.Register(api, huma.Operation{
		OperationID: "login",
		Method:      http.MethodPost,
		Path:        "/api/login",
		Summary:     "Login a user",
		Description: "Authenticate a user by username + password. The response body carries the access token, refresh token, token type, expiry, and user profile.",
		Tags:        []string{"auth"},
		// Same per-route limiter as register — login is the higher-
		// value target so the cap matters more here than there.
		Middlewares: huma.Middlewares{middleware.DefaultAuthRateLimit()},
	}, h.Login)

	// /api/refresh is public: it does not require an access token.
	// The refresh token rides in the request body; the handler
	// rotates the family and re-issues a fresh pair in the
	// response body.
	huma.Register(api, huma.Operation{
		OperationID: "refresh",
		Method:      http.MethodPost,
		Path:        "/api/refresh",
		Summary:     "Refresh access token",
		Description: "Exchange a valid refresh token (in the request body) for a fresh access + refresh pair. Returns 401 on invalid / expired / reused tokens; reuse triggers family-wide revocation.",
		Tags:        []string{"auth"},
	}, h.Refresh)

	// /api/logout IS auth-required: the middleware enforces a valid
	// Authorization: Bearer header. The refresh_token in the body
	// identifies the single refresh-token row we revoke
	// (per-session — other devices stay logged in, see SECURITY.md M1).
	huma.Register(api, huma.Operation{
		OperationID: "logout",
		Method:      http.MethodPost,
		Path:        "/api/logout",
		Summary:     "Logout a user",
		Description: "Revoke the refresh token identified in the request body. Requires a valid access token in the Authorization header. Returns 204.",
		Tags:        []string{"auth"},
		Security:    []map[string][]string{{"BearerAuth": {}}},
		Middlewares: huma.Middlewares{middleware.NewHumaAuth(tokens)},
	}, h.Logout)

	// /api/me returns the profile of the access token's subject.
	// The SPA fires this on every page load so its persisted
	// user can be reconciled against the server-truth (SECURITY.md L7).
	huma.Register(api, huma.Operation{
		OperationID: "me",
		Method:      http.MethodGet,
		Path:        "/api/me",
		Summary:     "Get current user",
		Description: "Return the profile of the authenticated user (the access token's subject). Used by the SPA to reconcile its persisted user on every page load.",
		Tags:        []string{"auth"},
		Security:    []map[string][]string{{"BearerAuth": {}}},
		Middlewares: huma.Middlewares{middleware.NewHumaAuth(tokens)},
	}, h.Me)
}

// ---- Operation input / output structs ----

type registerInput struct{ Body RegisterRequest }

// registerOutput carries the JSON body — the access token, refresh
// token, token type, expiry, and user profile. No Set-Cookie
// headers; clients store the tokens themselves and stamp them on
// subsequent requests via Authorization: Bearer.
type registerOutput struct {
	Status int          `status:"201"`
	Body   AuthResponse `nullable:"false"`
}

type loginInput struct{ Body LoginRequest }

type loginOutput struct {
	Body AuthResponse `nullable:"false"`
}

// refreshInput carries the refresh token in the body — the same
// Bearer contract as every other auth surface.
type refreshInput struct{ Body RefreshRequest }

// refreshOutput returns the same AuthResponse shape as
// login/register. Clients treat /api/refresh as a credential-
// exchange endpoint — they don't care that the underlying row was
// rotated, just that the new pair is in the body.
type refreshOutput struct {
	Body AuthResponse `nullable:"false"`
}

// logoutInput carries the refresh token in the body so the handler
// can revoke exactly that row.
type logoutInput struct{ Body LogoutRequest }

// logoutOutput returns 204 with no body. With only Status set,
// huma generates a 204 response with no content schema (mirroring
// deleteMovieOutput in internal/movie/huma_handler.go).
type logoutOutput struct {
	Status int `status:"204"`
}

// meOutput returns just the user profile — no tokens, no
// ExpiresAt. The body shape matches what the SPA already consumes
// from login/register/refresh so the frontend can drop it straight
// into the authStore. SECURITY.md L7.
type meOutput struct {
	Body User `nullable:"false"`
}

// ---- Handler functions ----

//nolint:revive // unexported-return is huma's idiomatic op pattern
func (h *Handler) Register(ctx context.Context, in *registerInput) (*registerOutput, error) {
	res, err := h.service.Register(ctx, in.Body)
	if err != nil {
		return nil, api.MapError(ctx, err, "Failed to register user")
	}
	return &registerOutput{
		Status: http.StatusCreated,
		Body:   res.toResponse(),
	}, nil
}

//nolint:revive // unexported-return is huma's idiomatic op pattern
func (h *Handler) Login(ctx context.Context, in *loginInput) (*loginOutput, error) {
	res, err := h.service.Login(ctx, in.Body)
	if err != nil {
		return nil, api.MapError(ctx, err, "Failed to login")
	}
	return &loginOutput{Body: res.toResponse()}, nil
}

// Refresh handler. The three sentinel errors all map to 401 with
// distinct static messages; reuse vs. expired are distinguishable for
// clients that care (reuse = "your token was already used, please
// re-login"; expired = "your session timed out, please re-login") but
// a defensive client can collapse them. MapError is the only error
// path. On success the handler returns a fresh Bearer pair in the
// response body.
//
//nolint:revive // unexported-return is huma's idiomatic op pattern
func (h *Handler) Refresh(ctx context.Context, in *refreshInput) (*refreshOutput, error) {
	if in.Body.RefreshToken == "" {
		return nil, api.MapError(ctx, ErrInvalidRefreshToken, "Failed to refresh token")
	}
	res, err := h.service.Refresh(ctx, in.Body.RefreshToken)
	if err != nil {
		return nil, api.MapError(ctx, err, "Failed to refresh token")
	}
	return &refreshOutput{Body: res.toResponse()}, nil
}

// Logout handler. The middleware (NewHumaAuth) has already verified
// the access token in the Authorization header; the refresh_token
// in the body identifies the single refresh-token row we revoke
// (per-session — other devices stay logged in, see SECURITY.md M1).
// Returns 204 with no body. When the refresh token is missing /
// invalid the row revoke is skipped — the access token is already
// invalid by definition if we got here, so the cookie-based escape
// hatch no longer applies.
//
//nolint:revive // unexported-return is huma's idiomatic op pattern
func (h *Handler) Logout(ctx context.Context, in *logoutInput) (*logoutOutput, error) {
	if in.Body.RefreshToken != "" {
		if err := h.service.Logout(ctx, in.Body.RefreshToken); err != nil {
			return nil, api.MapError(ctx, err, "Failed to logout")
		}
	}
	return &logoutOutput{Status: http.StatusNoContent}, nil
}

// Me returns the user profile for the access token's subject.
// SECURITY.md L7: the SPA fires /api/me on every page load to
// reconcile its persisted user. The auth middleware has already
// validated the access token and stashed the subject id on the
// context — if it didn't, this handler wouldn't run. A 404
// (ErrUserNotFound) is theoretically possible only if the row
// was deleted between login and now; api.MapError returns the
// standard 404 envelope in that case.
//
//nolint:revive // unexported-return is huma's idiomatic op pattern
func (h *Handler) Me(ctx context.Context, _ *struct{}) (*meOutput, error) {
	id := reqctx.UserIDFromContext(ctx)
	u, err := h.service.GetByID(ctx, id)
	if err != nil {
		return nil, api.MapError(ctx, err, "Failed to load user")
	}
	return &meOutput{Body: *u}, nil
}
