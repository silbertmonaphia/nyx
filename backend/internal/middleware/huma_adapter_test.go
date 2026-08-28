package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"nyx/internal/platform/auth"
	"nyx/internal/reqctx"
)

// guardedOutput is the huma-shaped response for the /guarded test
// endpoint. Wrapping the body in an output struct (rather than
// returning *struct{} directly) is what tells huma to serialise the
// payload with 200 OK — a bare *struct{} falls through to the 204
// no-content default.
type guardedOutput struct {
	Body guardedBody
}

type guardedBody struct {
	UserID   int    `json:"user_id"`
	Username string `json:"username"`
}

// newGuardedRouter builds a minimal huma API with a single /guarded
// operation guarded by NewHumaAuth. The returned *chi.Mux is what
// tests exercise via httptest. StoreRequest is installed so the
// auth middleware can recover the live *http.Request from the
// huma.Context (it reads the Authorization header directly).
func newGuardedRouter(t *testing.T, tokens auth.TokenService) http.Handler {
	t.Helper()
	router := chi.NewMux()
	router.Use(StoreRequest)
	hapi := humachi.New(router, huma.Config{
		OpenAPI: &huma.OpenAPI{
			OpenAPI: "3.1.0",
			Info:    &huma.Info{Title: "huma auth test", Version: "0.0.0"},
		},
		Formats:       huma.DefaultFormats,
		DefaultFormat: "application/json",
	})
	huma.Register(hapi, huma.Operation{
		OperationID: "guarded",
		Method:      http.MethodGet,
		Path:        "/guarded",
		Middlewares: huma.Middlewares{NewHumaAuth(tokens)},
	}, func(ctx context.Context, _ *struct{}) (*guardedOutput, error) {
		// Echo the resolved identity back as a marker so the test
		// can assert the context stamping without peeking at private
		// middleware internals.
		return &guardedOutput{Body: guardedBody{
			UserID:   reqctx.UserIDFromContext(ctx),
			Username: reqctx.UsernameFromContext(ctx),
		}}, nil
	})
	return router
}

// TestHumaAuth_BearerHeaderAccepted pins the Bearer-on-the-wire
// contract on the huma-shaped middleware. When ValidateToken
// succeeds, the guarded handler runs and the resolved user identity
// reaches the handler via reqctx — this is the load-bearing path
// for native-mobile, Unity / Unreal game, and console clients that
// stamp Authorization: Bearer themselves.
func TestHumaAuth_BearerHeaderAccepted(t *testing.T) {
	tokens := &happyTokens{}

	router := newGuardedRouter(t, tokens)
	req := httptest.NewRequest(http.MethodGet, "/guarded", nil)
	req.Header.Set("Authorization", "Bearer valid.jwt.token")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("WWW-Authenticate"); got != "" {
		t.Errorf("WWW-Authenticate = %q, want empty on success", got)
	}
	if !contains(rr.Body.String(), `"user_id":42`) || !contains(rr.Body.String(), `"username":"alice"`) {
		t.Errorf("downstream handler did not see resolved claims; body=%s", rr.Body.String())
	}
}

// TestHumaAuth_ClaimsStampedOnContext pins the identity-propagation
// contract on the huma path: the resolved user_id and username
// reach the handler via reqctx.UserIDFromContext /
// UsernameFromContext. Downstream handlers (Me, Logout, movie ops)
// read these to authorise their work — if the stamping breaks, the
// handler would still run (with uid=0) and silently corrupt state.
func TestHumaAuth_ClaimsStampedOnContext(t *testing.T) {
	tokens := &happyTokens{}

	router := newGuardedRouter(t, tokens)
	req := httptest.NewRequest(http.MethodGet, "/guarded", nil)
	req.Header.Set("Authorization", "Bearer valid.jwt.token")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if !contains(rr.Body.String(), `"user_id":42`) {
		t.Errorf("expected user_id=42 in response, body=%s", rr.Body.String())
	}
	if !contains(rr.Body.String(), `"username":"alice"`) {
		t.Errorf("expected username=alice in response, body=%s", rr.Body.String())
	}
}

// contains is a tiny helper to keep test bodies readable; avoids
// pulling in strings for a one-off substring check.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
