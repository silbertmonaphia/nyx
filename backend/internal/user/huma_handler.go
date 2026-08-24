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
// main.go from a Service and registered onto a huma API via
// RegisterUserOps.
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
// token, and the handler revokes the entire refresh-token family
// identified by the body's refresh_token.
//
// tokens is threaded through to attach middleware.NewHumaAuth to the
// protected logout operation.
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
		Description: "Create a new user account. Returns 409 when the username or email is already taken.",
		Tags:        []string{"auth"},
	}, h.Register)

	huma.Register(api, huma.Operation{
		OperationID: "login",
		Method:      http.MethodPost,
		Path:        "/api/login",
		Summary:     "Login a user",
		Description: "Authenticate a user by username + password and receive a JWT.",
		Tags:        []string{"auth"},
	}, h.Login)

	// /api/refresh is public: the access token in the Authorization
	// header is intentionally NOT required. The client sends its
	// refresh_token (which it may have received hours ago, well past
	// the access-token lifetime) and gets back a fresh pair.
	huma.Register(api, huma.Operation{
		OperationID: "refresh",
		Method:      http.MethodPost,
		Path:        "/api/refresh",
		Summary:     "Refresh access token",
		Description: "Exchange a valid refresh token for a fresh access + refresh pair. Returns 401 on invalid / expired / reused tokens; reuse triggers family-wide revocation.",
		Tags:        []string{"auth"},
	}, h.Refresh)

	// /api/logout IS auth-required: the caller must already hold a
	// valid access token to identify themselves. The body's
	// refresh_token is what we revoke. The empty 204 response
	// mirrors the gin-era convention; client logout UX is unchanged.
	huma.Register(api, huma.Operation{
		OperationID:          "logout",
		Method:               http.MethodPost,
		Path:                 "/api/logout",
		Summary:              "Logout a user",
		Description:          "Revoke the supplied refresh token's entire family. Requires a valid access token in the Authorization header. Returns 204.",
		Tags:                 []string{"auth"},
		Security:             []map[string][]string{{"BearerAuth": {}}},
		Middlewares:          huma.Middlewares{middleware.NewHumaAuth(tokens)},
	}, h.Logout)
}

// ---- Operation input / output structs ----

type registerInput struct{ Body RegisterRequest }

type registerOutput struct {
	Status int `status:"201"`
	Body   AuthResponse
}

type loginInput struct{ Body LoginRequest }

type loginOutput struct {
	Body AuthResponse
	// 200 is the default; huma uses DefaultStatus unless overridden.
}

type refreshInput struct{ Body RefreshRequest }

// refreshOutput deliberately returns the same AuthResponse shape as
// login/register. Clients treat /api/refresh as a credential-exchange
// endpoint — they don't care that the underlying row was rotated, just
// that they have a fresh pair to use.
type refreshOutput struct {
	Body AuthResponse
}

type logoutInput struct {
	Body LogoutRequest
}

// logoutOutput intentionally has no Body field. With only the Status
// field present, huma generates a 204 response with no content
// schema in the OpenAPI spec (instead of the 200 + body shape the
// previous version emitted, which lied to consumers). Mirrors
// deleteMovieOutput in internal/movie/huma_handler.go.
type logoutOutput struct {
	Status int `status:"204"`
}

// ---- Handler functions ----

func (h *Handler) Register(ctx context.Context, in *registerInput) (*registerOutput, error) {
	res, err := h.service.Register(ctx, in.Body)
	if err != nil {
		return nil, api.MapError(ctx, err, "Failed to register user")
	}
	return &registerOutput{Status: http.StatusCreated, Body: *res}, nil
}

func (h *Handler) Login(ctx context.Context, in *loginInput) (*loginOutput, error) {
	res, err := h.service.Login(ctx, in.Body)
	if err != nil {
		return nil, api.MapError(ctx, err, "Failed to login")
	}
	return &loginOutput{Body: *res}, nil
}

// Refresh handler. The three sentinel errors all map to 401 with
// distinct static messages; reuse vs. expired are distinguishable for
// clients that care (reuse = "your token was already used, please
// re-login"; expired = "your session timed out, please re-login") but
// a defensive client can collapse them. MapError is the only error
// path.
func (h *Handler) Refresh(ctx context.Context, in *refreshInput) (*refreshOutput, error) {
	res, err := h.service.Refresh(ctx, in.Body)
	if err != nil {
		return nil, api.MapError(ctx, err, "Failed to refresh token")
	}
	return &refreshOutput{Body: *res}, nil
}

// Logout handler. The middleware (NewHumaAuth) has already verified
// the access token before we get here; the body supplies the
// refresh_token whose family we revoke. Returns 204 with no body —
// matching the gin-era contract the frontend already expects.
func (h *Handler) Logout(ctx context.Context, in *logoutInput) (*logoutOutput, error) {
	if err := h.service.Logout(ctx, in.Body); err != nil {
		return nil, api.MapError(ctx, err, "Failed to logout")
	}
	return &logoutOutput{Status: http.StatusNoContent}, nil
}
