package api

import "github.com/danielgtaylor/huma/v2"

// HumaConfig returns the huma.Config shared by every entrypoint that
// builds the Nyx API surface: the server in cmd/api and the offline
// spec dumper in cmd/openapi. Keeping it here means the committed
// OpenAPI artifact cannot drift from what the server actually serves.
//
// A fresh huma.OpenAPI (and its nested maps) is allocated on every call
// — huma mutates the struct as operations are registered, so callers
// must not share one.
//
// OpenAPIPath is the *base* path: huma appends the format extension
// itself, registering /api/swagger/doc.json, /api/swagger/doc.yaml and
// their 3.0 downgrade variants. Do not write ".json" here or the routes
// become /api/swagger/doc.json.json.
func HumaConfig() huma.Config {
	return huma.Config{
		OpenAPI: &huma.OpenAPI{
			OpenAPI: "3.1.0",
			Info: &huma.Info{
				Title:       "Nyx API",
				Version:     "1.0.0",
				Description: "Minimalist media rating application API.",
			},
			Components: &huma.Components{
				SecuritySchemes: map[string]*huma.SecurityScheme{
					"BearerAuth": {
						Type:         "http",
						Scheme:       "bearer",
						BearerFormat: "JWT",
						Description:  "JWT bearer token issued by POST /api/login or POST /api/register.",
					},
				},
			},
		},
		OpenAPIPath:   "/api/swagger/doc",
		DocsPath:      "/api/swagger",
		Formats:       huma.DefaultFormats,
		DefaultFormat: "application/json",
	}
}
