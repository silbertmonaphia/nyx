// Package observability bootstraps the OpenTelemetry tracing SDK.
//
// Setup is the only entry point main.go needs to call. When
// cfg.OTelEnabled is false it returns a Tracing whose Provider is
// the global noop provider and whose Shutdown is a no-op, so callers
// can defer it unconditionally without paying any cost in unit
// tests or testcontainers.
//
// When enabled, Setup wires an OTLP/HTTP exporter pointed at
// cfg.OTelExporterOTLPEndpoint with a BatchSpanProcessor (5s flush,
// 512-span queue). Sampling defaults to ParentBased(AlwaysOn); the
// SDK honours the standard OTEL_TRACES_SAMPLER / OTEL_TRACES_SAMPLER_ARG
// env vars natively so this code does not re-implement them.
package observability

import (
	"context"
	"fmt"
	"time"

	"nyx/internal/platform/config"

	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// ServiceVersion is stamped on every span as service.version. Set via
// -ldflags at build time (e.g. -ldflags "-X .../observability.ServiceVersion=v1.2.3").
// Falls back to "dev" for `go run` and unit tests.
var ServiceVersion = "dev"

// Tracing bundles the tracer provider with its shutdown closure so
// the caller can defer the flush without juggling two return values.
//
// Provider is the otel.TracerProvider to install as the global
// (otel.SetTracerProvider) and to hand out via .Tracer("...") for
// per-domain spans (movie, user, pgx).
//
// Shutdown flushes any buffered spans; safe to call when tracing is
// disabled (returns nil immediately).
type Tracing struct {
	Provider trace.TracerProvider
	Shutdown func(context.Context) error
}

// Setup initialises the tracer provider. Returns a Tracing whose
// Provider is the noop provider when cfg.OTelEnabled is false. The
// caller MUST call otel.SetTracerProvider on the returned Provider
// (and otel.SetTextMapPropagator for the W3C TraceContext + Baggage
// composite propagator) before any other code reads the global
// tracer — observability does not touch globals so unit tests can
// install their own providers without interference.
func Setup(ctx context.Context, cfg *config.Config) (*Tracing, error) {
	if !cfg.OTelEnabled {
		return &Tracing{
			Provider: noop.NewTracerProvider(),
			Shutdown: func(context.Context) error { return nil },
		}, nil
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(cfg.OTelServiceName),
			semconv.ServiceVersion(ServiceVersion),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("observability: build resource: %w", err)
	}

	// URL scheme drives Insecure: WithEndpointURL parses the URL and
	// sets Traces.Insecure based on whether the scheme is "https" or
	// "http" — we do NOT append a separate WithInsecure() here, since
	// doing so after WithEndpointURL would silently downgrade an
	// https URL to plaintext if a separate flag were ever flipped on.
	exporter, err := otlptrace.New(ctx, otlptracehttp.NewClient(
		otlptracehttp.WithEndpointURL(cfg.OTelExporterOTLPEndpoint),
	))
	if err != nil {
		return nil, fmt.Errorf("observability: create OTLP exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter,
			sdktrace.WithBatchTimeout(5*time.Second), // explicit; matches BatchSpanProcessor default
			sdktrace.WithMaxQueueSize(512),
		),
		sdktrace.WithResource(res),
		// ParentBased(AlwaysOn) — root spans are always sampled; child
		// spans follow their parent. SDK still honours OTEL_TRACES_SAMPLER
		// at construction time, so operators can flip to traceidratio
		// without recompiling.
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
	)

	log.Info().
		Str("endpoint", cfg.OTelExporterOTLPEndpoint).
		Str("service_name", cfg.OTelServiceName).
		Msg("OpenTelemetry tracing enabled")

	return &Tracing{
		Provider: tp,
		Shutdown: tp.Shutdown,
	}, nil
}
