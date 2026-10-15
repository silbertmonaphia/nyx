package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"nyx/internal/platform/api"

	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
)

// TestHumaConfigSwaggerRoutes pins the documented swagger URLs.
//
// OpenAPIPath is the *base* path — huma appends the format extension
// itself. Writing "/api/swagger/doc.json" there (as an earlier revision
// did) silently produced "/api/swagger/doc.json.json" and 404'd the URL
// every doc in the repo points at. This test fails if that regresses.
func TestHumaConfigSwaggerRoutes(t *testing.T) {
	t.Parallel()

	router := chi.NewRouter()
	humachi.New(router, api.HumaConfig())

	t.Run("documented paths resolve", func(t *testing.T) {
		t.Parallel()
		for _, path := range []string{
			"/api/swagger",
			"/api/swagger/doc.json",
			"/api/swagger/doc.yaml",
		} {
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusOK {
				t.Errorf("GET %s = %d, want 200", path, rec.Code)
			}
		}
	})

	t.Run("double extension does not resolve", func(t *testing.T) {
		t.Parallel()
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/swagger/doc.json.json", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET /api/swagger/doc.json.json = %d, want 404 (OpenAPIPath must not carry an extension)", rec.Code)
		}
	})
}

// TestHumaConfigIsolated guards against a shared, mutable OpenAPI
// document leaking between callers. huma mutates Config.OpenAPI as
// operations are registered, so cmd/api and cmd/openapi must each get
// their own.
func TestHumaConfigIsolated(t *testing.T) {
	t.Parallel()

	a, b := api.HumaConfig(), api.HumaConfig()
	if a.OpenAPI == b.OpenAPI {
		t.Fatal("HumaConfig returned a shared *huma.OpenAPI; each call must allocate its own")
	}
	if a.OpenAPI.Components.SecuritySchemes["BearerAuth"] == nil {
		t.Error("BearerAuth security scheme is missing from the config")
	}
}
