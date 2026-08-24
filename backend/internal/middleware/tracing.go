package middleware

import (
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Tracing returns a middleware that opens an OTel server span per
// HTTP request. Span name is the chi route template (e.g.
// "GET /api/movies/{id}"), falling back to the raw URL path for
// requests that didn't match any registered route. Using the
// template keeps span cardinality bounded — Jaeger / Tempo don't
// explode into one series per id.
//
// Place this BEFORE RequestID in the chain so the server span is
// the parent of any child span the application opens (service,
// repository, pgx). When OTEL_ENABLED is false, the global
// TracerProvider is the noop provider and otelhttp still runs but
// every Start returns a non-recording span — the middleware stays
// in the chain unconditionally and never needs to be unwired.
//
// serviceName is stamped on every span as the OTel resource
// attribute service.name (via the underlying otelhttp handler) so
// Jaeger can group by service. Defaults to "nyx-backend" when
// main.go passes the configured value.
func Tracing(serviceName string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return otelhttp.NewHandler(next, serviceName,
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
				route := routePattern(r)
				if route == "" {
					// Unmatched route (404): fall back to the raw path so
					// the span is still useful when debugging.
					route = r.URL.Path
				}
				return r.Method + " " + route
			}),
		)
	}
}
