// Package telemetry provides OpenTelemetry tracing initialization for Velox.
//
// Configure via environment:
//
//	OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318  (Jaeger, Grafana Tempo, etc.)
//	OTEL_SERVICE_NAME=velox                            (defaults to "velox")
//
// When OTEL_EXPORTER_OTLP_ENDPOINT is not set, tracing is disabled (noop).
package telemetry

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// Init sets up the global OpenTelemetry tracer provider.
// Returns a shutdown function that must be called on application exit.
// If OTEL_EXPORTER_OTLP_ENDPOINT is not set, returns a noop — zero overhead.
func Init(ctx context.Context) (shutdown func(context.Context) error, err error) {
	endpoint := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	if endpoint == "" {
		slog.Info("tracing disabled (OTEL_EXPORTER_OTLP_ENDPOINT not set)")
		return func(context.Context) error { return nil }, nil
	}

	serviceName := strings.TrimSpace(os.Getenv("OTEL_SERVICE_NAME"))
	if serviceName == "" {
		serviceName = "velox"
	}

	// Schemaless on purpose. resource.Merge REFUSES to merge two resources
	// carrying different schema URLs, and resource.Default() reports whatever
	// schema the SDK ships with — so pairing it with a pinned
	// semconv/vX.Y.Z resource breaks the moment the SDK's schema moves ahead
	// of the import. It had: the SDK sits at 1.44.0 (schema 1.41.0) while the
	// import pinned 1.27.0, and Init returned
	// "conflicting Schema URL: …/1.41.0 and …/1.27.0". That error is fatal at
	// boot, so simply setting OTEL_EXPORTER_OTLP_ENDPOINT — the documented way
	// to turn tracing on — stopped the server from starting at all.
	//
	// A schemaless resource merges with anything, so the attribute lands and
	// Default()'s schema is preserved. This cannot drift again on an SDK bump;
	// re-pinning semconv to match would only reset the clock.
	res, err := resource.Merge(
		resource.Default(),
		resource.NewSchemaless(
			semconv.ServiceName(serviceName),
		),
	)
	if err != nil {
		return nil, err
	}

	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(1.0))),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	slog.Info("tracing enabled", "endpoint", endpoint, "service", serviceName)
	return tp.Shutdown, nil
}

// Tracer returns a named tracer for the given package/component.
// Safe to call even when tracing is disabled (returns noop tracer).
func Tracer(name string) trace.Tracer {
	return otel.Tracer(name)
}

// NoopTracer returns a tracer that does nothing (for tests).
func NoopTracer() trace.Tracer {
	return noop.NewTracerProvider().Tracer("")
}
