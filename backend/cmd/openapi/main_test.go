package main

import (
	"encoding/json"
	"testing"
)

// TestGenerateSpec asserts the offline spec dump builds without a
// database and covers every registered route. It is the guard against a
// future entrypoint change (e.g. dropping OverrideHumaErrors) silently
// producing a wrong committed artifact.
func TestGenerateSpec(t *testing.T) {
	raw, err := generateSpec()
	if err != nil {
		t.Fatalf("generateSpec: %v", err)
	}

	var doc struct {
		OpenAPI    string                     `json:"openapi"`
		Paths      map[string]json.RawMessage `json:"paths"`
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal spec: %v", err)
	}

	if doc.OpenAPI != "3.1.0" {
		t.Errorf("openapi version = %q, want 3.1.0", doc.OpenAPI)
	}

	for _, p := range []string{
		"/api/health",
		"/api/movies",
		"/api/movies/{id}",
		"/api/login",
		"/api/register",
	} {
		if _, ok := doc.Paths[p]; !ok {
			t.Errorf("spec is missing path %q", p)
		}
	}

	// OverrideHumaErrors must have run before registration, otherwise
	// huma emits its own ErrorModel schema instead of ours.
	if _, ok := doc.Components.Schemas["ErrorResponse"]; !ok {
		t.Error("spec is missing the ErrorResponse schema; was OverrideHumaErrors called first?")
	}
	if _, ok := doc.Components.Schemas["ErrorModel"]; ok {
		t.Error("spec contains huma's default ErrorModel schema")
	}
}

// TestGenerateSpecDeterministic guards the drift check: two runs must
// produce byte-identical output or `make openapi-diff` becomes flaky.
func TestGenerateSpecDeterministic(t *testing.T) {
	first, err := generateSpec()
	if err != nil {
		t.Fatalf("generateSpec (first): %v", err)
	}
	second, err := generateSpec()
	if err != nil {
		t.Fatalf("generateSpec (second): %v", err)
	}
	if string(first) != string(second) {
		t.Error("generateSpec output is not deterministic")
	}
}
