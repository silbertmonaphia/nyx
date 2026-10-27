package llmsettings

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
)

type ssrfFixture struct {
	Description string `json:"description"`
	Cases       []struct {
		URL   string `json:"url"`
		Field string `json:"field"`
	} `json:"cases"`
}

func loadSSRFFixture(t *testing.T) ssrfFixture {
	t.Helper()
	data, err := os.ReadFile("../../agent_py/tests/fixtures/ssrf_denied_urls.json")
	if err != nil {
		t.Fatalf("read shared ssrf fixture: %v", err)
	}
	var f ssrfFixture
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("parse shared ssrf fixture: %v", err)
	}
	if len(f.Cases) == 0 {
		t.Fatal("shared ssrf fixture has no cases")
	}
	return f
}

func TestValidateUpdate_SharedDeniedURLFixture(t *testing.T) {
	fixture := loadSSRFFixture(t)
	for _, tc := range fixture.Cases {
		in := validPublicCreateInput()
		switch tc.Field {
		case "llm_base_url":
			in.LLMBaseURL = tc.URL
		case "embedding_base_url":
			in.EmbeddingBaseURL = tc.URL
		case "fallback_base_url":
			in.FallbackBaseURL = tc.URL
			in.FallbackModel = "fallback-model"
			in.FallbackAPIKey = "sk-fallback-key-7777"
		case "tavily_base_url":
			in.TavilyBaseURL = tc.URL
		default:
			t.Fatalf("unknown field %q in fixture", tc.Field)
		}
		err := ValidateUpdate(&in, nil)
		if err == nil {
			t.Fatalf("%s: URL %s must be refused by the default policy", tc.Field, tc.URL)
		}
		var fe *InvalidFieldError
		if !errors.As(err, &fe) || fe.Field != tc.Field {
			t.Fatalf("%s: URL %s must be rejected naming %s, got %v", tc.Field, tc.URL, tc.Field, err)
		}
	}
}
