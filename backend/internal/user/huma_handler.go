package user

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"nyx/internal/middleware"
	"nyx/internal/platform/api"
	"nyx/internal/platform/auth"
)

// Handler exposes user/auth domain operations. It is constructed in
// main.go from a Service + CookieConfig and registered onto a huma
// API via RegisterUserOps. cookies carries the cookie attributes
// every auth handler needs to Set / Clear — centralising the lookup
// in Handler keeps SetAuthCookies / ClearAuthCookies out of the
// individual handler bodies.
type Handler struct {
	service Service
	cookies auth.CookieConfig
}

func NewHandler(service Service, cookies auth.CookieConfig) *Handler {
	return &Handler{service: service, cookies: cookies}
}

// RegisterUserOps wires the user endpoints onto a huma API. The
// register/refresh routes are public — no Middlewares — so the
// existing rate-limit cap is the only upstream gate. The /api/logout
// route is auth-required: callers must already hold a valid access
// token (in the __Host-nyx-access cookie OR, during the rollout
// window, in the Authorization header), and the handler revokes the
// entire refresh-token family identified by the __Host-nyx-refresh
// cookie.
//
// tokens is threaded through to attach middleware.NewHumaAuth to the
// protected logout operation. cookies is the shared CookieConfig
// every handler needs.
func RegisterUserOps(api huma.API, h *Handler, tokens auth.TokenService, cookies auth.CookieConfig) {
	RegisterUserOpsTest(api, h, tokens, cookies)
}

// RegisterUserOpsTest registers the same operations as RegisterUserOps.
// It exists so handler tests can build a lean chi + huma stack against
// the exact production operation set (mirroring movie's
// RegisterMovieOpsTest). Unlike movie there is no `withAuth` flag:
// register, login, and refresh are public by design, so there is no
// JWT middleware to toggle. Logout is always auth-required.
func RegisterUserOpsTest(api huma.API, h *Handler, tokens auth.TokenService, cookies auth.CookieConfig) {
	huma.Register(api, huma.Operation{
		OperationID: "register",
		Method:      http.MethodPost,
		Path:        "/api/register",
		Summary:     "Register a user",
		Description: "Create a new user account. Returns 409 when the username or email is already taken. Sets __Host-nyx-access and __Host-nyx-refresh httpOnly cookies via Set-Cookie headers.",
		Tags:        []string{"auth"},
	}, h.Register)

	huma.Register(api, huma.Operation{
		OperationID: "login",
		Method:      http.MethodPost,
		Path:        "/api/login",
		Summary:     "Login a user",
		Description: "Authenticate a user by username + password. Sets __Host-nyx-access and __Host-nyx-refresh httpOnly cookies; the JSON body contains only the user profile.",
		Tags:        []string{"auth"},
	}, h.Login)

	// /api/refresh is public: it does not require an access token.
	// The __Host-nyx-refresh cookie carries the credential. We rotate
	// the family and re-issue both cookies in the response.
	huma.Register(api, huma.Operation{
		OperationID: "refresh",
		Method:      http.MethodPost,
		Path:        "/api/refresh",
		Summary:     "Refresh access token",
		Description: "Exchange a valid refresh cookie for a fresh access + refresh pair. Returns 401 on invalid / expired / reused tokens; reuse triggers family-wide revocation. Both cookies are re-issued.",
		Tags:        []string{"auth"},
	}, h.Refresh)

	// /api/logout IS auth-required: the middleware enforces a valid
	// access cookie (or Authorization header during the rollout
	// window). The __Host-nyx-refresh cookie carries the family we
	// revoke. Returns 204 and clears both auth cookies.
	huma.Register(api, huma.Operation{
		OperationID: "logout",
		Method:      http.MethodPost,
		Path:        "/api/logout",
		Summary:     "Logout a user",
		Description: "Revoke the refresh token family identified by the __Host-nyx-refresh cookie. Requires a valid access token (cookie or Authorization header). Returns 204 and clears both auth cookies.",
		Tags:        []string{"auth"},
		Security:    []map[string][]string{{"BearerAuth": {}}},
		Middlewares: huma.Middlewares{middleware.NewHumaAuth(tokens, cookies.AccessName, cookies.Secure)},
	}, h.Logout)
}

// ---- Operation input / output structs ----

type registerInput struct{ Body RegisterRequest }

// registerOutput carries both the JSON body and the two Set-Cookie
// headers. huma emits []string tagged `header:"Set-Cookie"` as one
// header per slice element (via AppendHeader), so the browser
// stores both cookies.
type registerOutput struct {
	Status    int          `status:"201"`
	SetCookie []string     `header:"Set-Cookie"`
	Body      AuthResponse `nullable:"false"`
}

type loginInput struct{ Body LoginRequest }

type loginOutput struct {
	SetCookie []string     `header:"Set-Cookie"`
	Body      AuthResponse `nullable:"false"`
	// 200 is the default; huma uses DefaultStatus unless overridden.
}

// refreshInput has an empty body — the refresh token rides in the
// __Host-nyx-refresh cookie.
type refreshInput struct{ Body RefreshRequest }

// refreshOutput deliberately returns the same AuthResponse shape as
// login/register, plus re-issued Set-Cookie headers. Clients treat
// /api/refresh as a credential-exchange endpoint — they don't care
// that the underlying row was rotated, just that the cookies are
// fresh.
type refreshOutput struct {
	SetCookie []string     `header:"Set-Cookie"`
	Body      AuthResponse `nullable:"false"`
}

// logoutInput has an empty body — the refresh token rides in the
// __Host-nyx-refresh cookie.
type logoutInput struct{ Body LogoutRequest }

// logoutOutput carries the Set-Cookie clears (Max-Age=0) and the
// 204 status. With only Status set on the JSON envelope, huma
// generates a 204 response with no content schema (mirroring
// deleteMovieOutput in internal/movie/huma_handler.go).
type logoutOutput struct {
	Status    int      `status:"204"`
	SetCookie []string `header:"Set-Cookie"`
}

// ---- Handler functions ----

func (h *Handler) Register(ctx context.Context, in *registerInput) (*registerOutput, error) {
	res, err := h.service.Register(ctx, in.Body)
	if err != nil {
		return nil, api.MapError(ctx, err, "Failed to register user")
	}
	return &registerOutput{
		Status:    http.StatusCreated,
		SetCookie: issueSetCookieStrings(res, h.cookies),
		Body:      res.toResponse(),
	}, nil
}

func (h *Handler) Login(ctx context.Context, in *loginInput) (*loginOutput, error) {
	res, err := h.service.Login(ctx, in.Body)
	if err != nil {
		return nil, api.MapError(ctx, err, "Failed to login")
	}
	return &loginOutput{
		SetCookie: issueSetCookieStrings(res, h.cookies),
		Body:      res.toResponse(),
	}, nil
}

// Refresh handler. The three sentinel errors all map to 401 with
// distinct static messages; reuse vs. expired are distinguishable for
// clients that care (reuse = "your token was already used, please
// re-login"; expired = "your session timed out, please re-login") but
// a defensive client can collapse them. MapError is the only error
// path. On success the handler re-issues both auth cookies via the
// output struct's SetCookie field.
func (h *Handler) Refresh(ctx context.Context, in *refreshInput) (*refreshOutput, error) {
	raw, ok := refreshTokenFromContext(ctx, h.cookies)
	if !ok {
		return nil, api.MapError(ctx, ErrInvalidRefreshToken, "Failed to refresh token")
	}
	res, err := h.service.Refresh(ctx, raw)
	if err != nil {
		return nil, api.MapError(ctx, err, "Failed to refresh token")
	}
	return &refreshOutput{
		SetCookie: issueSetCookieStrings(res, h.cookies),
		Body:      res.toResponse(),
	}, nil
}

// Logout handler. The middleware (NewHumaAuth) has already verified
// the access token before we get here; the __Host-nyx-refresh cookie
// identifies the family we revoke. Returns 204 with no body and
// clears both auth cookies. Even when the refresh cookie is missing
// (e.g. an attacker stripped it) we still clear both at the browser
// — local clear is idempotent server-side.
func (h *Handler) Logout(ctx context.Context, in *logoutInput) (*logoutOutput, error) {
	if raw, ok := refreshTokenFromContext(ctx, h.cookies); ok {
		if err := h.service.Logout(ctx, raw); err != nil {
			return nil, api.MapError(ctx, err, "Failed to logout")
		}
	}
	return &logoutOutput{
		Status:    http.StatusNoContent,
		SetCookie: clearSetCookieStrings(h.cookies),
	}, nil
}
