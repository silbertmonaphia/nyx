// Command openapi dumps the Nyx OpenAPI 3.1 specification to a file (or
// stdout) without touching a database, Redis, or the network. The huma
// spec is derived purely from the registered operations' struct tags, so
// the handlers can be constructed with nil services — they are never
// invoked during generation.
//
// Usage:
//
//	go run ./cmd/openapi              # write to stdout
//	go run ./cmd/openapi ../api/openapi.json
//
// The output is deterministic (encoding/json sorts map keys), which is
// what lets `make openapi-diff` act as a drift check in CI, mirroring
// `make sqlc-diff`.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"nyx/internal/movie"
	"nyx/internal/platform/api"
	"nyx/internal/platform/auth"
	"nyx/internal/user"

	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
)

func main() {
	var out string
	if len(os.Args) > 1 {
		out = os.Args[1]
	}

	spec, err := generateSpec()
	if err != nil {
		fmt.Fprintf(os.Stderr, "openapi: %v\n", err)
		os.Exit(1)
	}

	if out == "" {
		if _, err := os.Stdout.Write(spec); err != nil {
			fmt.Fprintf(os.Stderr, "openapi: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// 0o644: the spec is a public, committed build artifact — it is meant
	// to be world-readable, unlike the 0o600 gosec's G306 assumes.
	if err := os.WriteFile(out, spec, 0o644); err != nil { //nolint:gosec // G306: public API spec artifact
		fmt.Fprintf(os.Stderr, "openapi: %v\n", err)
		os.Exit(1)
	}
}

// specSigningKey is an obviously-fake signing key used only to satisfy
// auth.NewTokenService, which refuses keys shorter than
// auth.MinSecretBytes (32). No token is ever minted or validated here —
// the TokenService is a constructor argument for the protected
// operations' middleware, and middleware does not run during spec
// generation. Keeping it inert and clearly labelled avoids any chance of
// a real key sneaking into a build artifact.
const specSigningKey = "nyx-openapi-generation-placeholder-value"

// generateSpec builds the huma API exactly the way cmd/api/main.go does
// and marshals the resulting OpenAPI document as indented JSON.
func generateSpec() ([]byte, error) {
	// Must run BEFORE humachi.New / huma.Register so the spec's error
	// schema is api.ErrorResponse rather than huma's RFC 9457 model.
	api.OverrideHumaErrors()

	tokens, err := auth.NewTokenService([]byte(specSigningKey), 15*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("building token service: %w", err)
	}

	humaAPI := humachi.New(chi.NewRouter(), api.HumaConfig())

	// nil services: registration only reads struct tags, the handler
	// funcs are never called.
	movie.RegisterMovieOps(humaAPI, movie.NewHandler(nil), tokens)
	user.RegisterUserOps(humaAPI, user.NewHandler(nil), tokens)

	b, err := json.MarshalIndent(humaAPI.OpenAPI(), "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshalling spec: %w", err)
	}
	return append(b, '\n'), nil
}
