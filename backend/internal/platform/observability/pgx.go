package observability

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// NewPgxTracer returns a pgx.QueryTracer that emits an OTel span per
// Query, QueryRow, and Exec call. Returns nil when provider is the
// noop provider so pgxpool takes its existing fast path (no
// interface dispatch, no allocations per query).
//
// Scope: only QueryTracer is implemented. BatchTracer (SendBatch),
// CopyFromTracer (CopyFrom), PrepareTracer (Prepare), and
// ConnectTracer (connect handshake) are intentionally not
// instrumented — sqlc generates Exec/Query/QueryRow throughout the
// hot path, and golang-migrate's SendBatch is a one-shot at boot
// rather than per-request. Revisit if SendBatch lands in user code.
//
// Span attributes follow the OTel database semantic conventions:
//   - db.system           = "postgresql"
//   - db.operation        = first SQL verb (SELECT, INSERT, …)
//   - db.response.status  = SQLSTATE on error (success → omit)
//
// db.statement is deliberately NOT recorded — pgx hands us the
// rendered SQL with parameters inlined, which can include PII (emails,
// passwords, refresh-token hashes). Recording only the verb keeps
// spans useful for debugging without leaking query contents.
func NewPgxTracer(provider trace.TracerProvider) pgx.QueryTracer {
	if _, ok := provider.(noop.TracerProvider); ok {
		return nil
	}
	return &pgxTracer{tracer: provider.Tracer("nyx.pgx")}
}

// pgxTracer implements pgx.QueryTracer.
type pgxTracer struct {
	tracer trace.Tracer
}

func (p *pgxTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	// span is intentionally not captured: the started span is
	// retrievable from the returned ctx (and from any ctx derived
	// from it) via trace.SpanFromContext. TraceQueryEnd uses that
	// retrieval so we don't have to thread the handle through pgx.
	ctx, _ = p.tracer.Start(ctx, "pgx.query",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			semconv.DBSystemPostgreSQL,
			semconv.DBOperationName(firstSQLVerb(data.SQL)),
		),
	)
	return ctx
}

func (p *pgxTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	span := trace.SpanFromContext(ctx)
	if data.Err != nil {
		span.SetStatus(codes.Error, data.Err.Error())
		var pgErr *pgconn.PgError
		if errors.As(data.Err, &pgErr) {
			span.SetAttributes(
				attribute.String("db.response.status_code", pgErr.Code),
				attribute.String("db.response.status", pgErr.SQLState()),
			)
		}
	}
	span.End()
}

// unknownVerb is the db.operation value when the SQL string is empty,
// whitespace-only, or starts with a non-keyword token (e.g. a comment
// with no following statement).
const unknownVerb = "UNKNOWN"

// firstSQLVerb returns the first keyword of the SQL statement,
// upper-cased. Returns unknownVerb for empty / whitespace-only
// strings. Used for the db.operation attribute; cheap parse via
// index lookup.
func firstSQLVerb(sql string) string {
	sql = strings.TrimLeft(sql, " \t\n\r")
	if sql == "" {
		return unknownVerb
	}
	// Strip leading line comments (-- …) so a header comment doesn't
	// mask the verb.
	for strings.HasPrefix(sql, "--") {
		if idx := strings.IndexByte(sql, '\n'); idx >= 0 {
			sql = strings.TrimLeft(sql[idx+1:], " \t\n\r")
		} else {
			return unknownVerb
		}
	}
	// Take the first run of letters / underscores.
	end := 0
	for end < len(sql) && (isLetter(sql[end]) || sql[end] == '_') {
		end++
	}
	if end == 0 {
		return unknownVerb
	}
	return strings.ToUpper(sql[:end])
}

func isLetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// Compile-time check: pgxTracer satisfies pgx.QueryTracer.
var _ pgx.QueryTracer = (*pgxTracer)(nil)
