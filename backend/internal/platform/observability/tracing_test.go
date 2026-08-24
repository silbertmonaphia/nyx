package observability

import (
	"context"
	"testing"

	"nyx/internal/platform/config"

	"go.opentelemetry.io/otel/trace/noop"
)

// TestSetup_DisabledIsNoop asserts that OTEL_ENABLED=false returns
// the noop provider and a no-op shutdown — callers can defer the
// shutdown unconditionally without paying for an OTLP client or
// sampler.
func TestSetup_DisabledIsNoop(t *testing.T) {
	cfg := &config.Config{OTelEnabled: false}

	got, err := Setup(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Setup(disabled): %v", err)
	}
	if _, ok := got.Provider.(noop.TracerProvider); !ok {
		t.Fatalf("disabled path: provider = %T, want noop.TracerProvider", got.Provider)
	}
	if err := got.Shutdown(context.Background()); err != nil {
		t.Fatalf("disabled path: Shutdown returned %v, want nil", err)
	}
}

// TestNewPgxTracer_NoopProviderReturnsNil ensures the pgx pool gets
// nil so it takes the existing fast path (no interface dispatch per
// query) when tracing is disabled.
func TestNewPgxTracer_NoopProviderReturnsNil(t *testing.T) {
	if got := NewPgxTracer(noop.NewTracerProvider()); got != nil {
		t.Fatalf("NewPgxTracer(noop) = %v, want nil", got)
	}
}

// TestFirstSQLVerb covers the cheap first-token parser used for the
// db.operation attribute. Cases: empty, leading whitespace, line
// comments, multi-statement SQL, non-letter first char.
func TestFirstSQLVerb(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "UNKNOWN"},
		{"   ", "UNKNOWN"},
		{"SELECT 1", "SELECT"},
		{"\n  INSERT INTO foo VALUES (1)", "INSERT"},
		{"-- header comment\nUPDATE bar SET x = 1", "UPDATE"},
		{"-- only comment", "UNKNOWN"},
		{"with cte AS (SELECT 1) SELECT * FROM cte", "WITH"},
		{"delete from x", "DELETE"},
		{"(SELECT 1)", "UNKNOWN"},            // non-letter first char
		{"1 + 1", "UNKNOWN"},                 // digit first char (psql expression)
		{"  ", "UNKNOWN"},                    // whitespace only after trim
		{"_internal_call()", "_INTERNAL_CALL"}, // underscore-prefixed
	}
	for _, tc := range cases {
		if got := firstSQLVerb(tc.in); got != tc.want {
			t.Errorf("firstSQLVerb(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
