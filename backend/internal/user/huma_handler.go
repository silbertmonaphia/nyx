package user

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/rs/zerolog/log"

	"nyx/internal/platform/api"
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
// register/login routes are public — no Middlewares — so the existing
// rate-limit cap is the only upstream gate.
func RegisterUserOps(api huma.API, h *Handler) {
	RegisterUserOpsTest(api, h)
}

// RegisterUserOpsTest registers the same operations as RegisterUserOps.
// It exists so handler tests can build a lean chi + huma stack against
// the exact production operation set (mirroring movie's
// RegisterMovieOpsTest). Unlike movie there is no `withAuth` flag:
// register and login are public by design, so there is no JWT
// middleware to toggle.
func RegisterUserOpsTest(api huma.API, h *Handler) {
	huma.Register(api, huma.Operation{
		OperationID: "register",
		Method:      http.MethodPost,
		Path:        "/api/register",
		Summary:     "Register a user",
		Description: "Create a new user account. Returns 409 when the username is already taken.",
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

// ---- Handler functions ----

func (h *Handler) Register(ctx context.Context, in *registerInput) (*registerOutput, error) {
	res, err := h.service.Register(ctx, in.Body)
	if err != nil {
		if errors.Is(err, ErrUserAlreadyExists) {
			return nil, &api.ErrorResponse{Message: "User already exists", Code: http.StatusConflict}
		}
		log.Error().Err(err).Msg("Error registering user")
		return nil, &api.ErrorResponse{
			Message: "Failed to register user",
			Code:    http.StatusInternalServerError,
			Details: err.Error(),
		}
	}
	return &registerOutput{Status: http.StatusCreated, Body: *res}, nil
}

func (h *Handler) Login(ctx context.Context, in *loginInput) (*loginOutput, error) {
	res, err := h.service.Login(ctx, in.Body)
	if err != nil {
		if errors.Is(err, ErrInvalidCredentials) {
			return nil, &api.ErrorResponse{Message: "Invalid credentials", Code: http.StatusUnauthorized}
		}
		log.Error().Err(err).Msg("Error logging in user")
		return nil, &api.ErrorResponse{
			Message: "Failed to login",
			Code:    http.StatusInternalServerError,
			Details: err.Error(),
		}
	}
	return &loginOutput{Body: *res}, nil
}
