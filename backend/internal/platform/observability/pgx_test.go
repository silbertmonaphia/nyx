package observability

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestNewPgxTracer_RealBranch verifies the non-noop branch of the
// constructor returns a usable pgx.QueryTracer (the noop-branch test
// lives in tracing_test.go).
func TestNewPgxTracer_RealBranch(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	tr := NewPgxTracer(tp)
	if tr == nil {
		t.Fatal("NewPgxTracer(non-noop) = nil, want a pgx.QueryTracer")
	}
}

// TestTraceQueryStartEnd_HappyPath drives TraceQueryStart and
// TraceQueryEnd through pgx's tracer contract and asserts the
// recorded span carries the expected name + db.* attributes.
func TestTraceQueryStartEnd_HappyPath(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	tr := NewPgxTracer(tp)

	ctx := tr.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{
		SQL: "SELECT id FROM movies WHERE id = $1",
	})
	tr.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{}) // no error

	if got := len(sr.Started()); got != 1 {
		t.Fatalf("spans started = %d, want 1", got)
	}
	span := sr.Started()[0]
	if name := span.Name(); name != "pgx.query" {
		t.Errorf("span.Name = %q, want %q", name, "pgx.query")
	}
	attrs := attrMap(span.Attributes())
	if got := attrs["db.system"]; got != "postgresql" {
		t.Errorf("db.system = %v, want postgresql", got)
	}
	if got := attrs["db.operation.name"]; got != "SELECT" {
		t.Errorf("db.operation.name = %v, want SELECT", got)
	}
}

// TestTraceQueryEnd_ErrorPgError ensures SQL errors carry the
// SQLSTATE / response code attributes via errors.As → *pgconn.PgError.
func TestTraceQueryEnd_ErrorPgError(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	tr := NewPgxTracer(tp)

	pgErr := &pgconn.PgError{Code: "23505", Message: "duplicate key"}
	wrapped := fmt.Errorf("insert: %w", pgErr)

	ctx := tr.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{
		SQL: "INSERT INTO movies (title) VALUES ($1)",
	})
	tr.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: wrapped})

	span := sr.Started()[0]
	attrs := attrMap(span.Attributes())
	if got := attrs["db.response.status_code"]; got != "23505" {
		t.Errorf("db.response.status_code = %v, want 23505", got)
	}
	if got := attrs["db.response.status"]; got != "23505" {
		t.Errorf("db.response.status = %v, want 23505", got)
	}
}

// TestTraceQueryEnd_ErrorNonPg verifies that a non-Postgres error does
// not add SQLSTATE attributes but still ends the span cleanly.
func TestTraceQueryEnd_ErrorNonPg(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	tr := NewPgxTracer(tp)

	ctx := tr.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{
		SQL: "SELECT 1",
	})
	tr.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: errors.New("boom")})

	span := sr.Started()[0]
	if _, ok := attrMap(span.Attributes())["db.response.status_code"]; ok {
		t.Errorf("db.response.status_code unexpectedly set on non-pg error")
	}
}

// attrMap flattens a slice of OTel attribute.KeyValue into a string-keyed
// map for ergonomic test assertions. String() returns the typed
// rendering for each value (e.g. "postgresql", "23505"), so the test
// stays robust to typed value changes (String, Int, etc.).
func attrMap(attrs []attribute.KeyValue) map[string]string {
	m := make(map[string]string, len(attrs))
	for _, a := range attrs {
		m[string(a.Key)] = a.Value.String()
	}
	return m
}
