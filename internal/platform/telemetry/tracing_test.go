package telemetry_test

import (
	"context"
	"testing"

	"github.com/sagarsuperuser/velox/internal/platform/telemetry"
)

// TestInit_SucceedsWhenEndpointConfigured is the regression guard for a
// failure that made tracing unusable without anyone noticing.
//
// Init builds its resource with resource.Merge(resource.Default(), …), and
// Merge REFUSES to combine two resources whose schema URLs differ.
// resource.Default() reports whatever schema the SDK ships with, so pairing
// it with a resource pinned to a specific semconv/vX.Y.Z broke as soon as the
// SDK moved ahead of the pin — otel v1.44.0 (schema 1.41.0) against an import
// of semconv/v1.27.0 produced:
//
//	conflicting Schema URL: …/1.41.0 and …/1.27.0
//
// That error is returned to main, which treats it as fatal, so merely SETTING
// OTEL_EXPORTER_OTLP_ENDPOINT stopped the server from booting at all. It
// failed closed and took the process with it rather than degrading to no
// tracing.
//
// No collector is required: the OTLP HTTP exporter connects lazily, so Init
// succeeding here is exactly the boot-time path.
func TestInit_SucceedsWhenEndpointConfigured(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4318")

	shutdown, err := telemetry.Init(context.Background())
	if err != nil {
		t.Fatalf("Init returned %v — with an endpoint configured this is fatal at boot, "+
			"so the server would refuse to start with tracing enabled", err)
	}
	if shutdown == nil {
		t.Fatal("Init returned a nil shutdown func")
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })
}

// TestInit_NoopWhenEndpointUnset pins the other half of the contract: with no
// endpoint configured tracing is disabled and Init must still hand back a
// usable no-op shutdown, so callers can defer it unconditionally.
func TestInit_NoopWhenEndpointUnset(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	shutdown, err := telemetry.Init(context.Background())
	if err != nil {
		t.Fatalf("Init with no endpoint should be a no-op, got %v", err)
	}
	if shutdown == nil {
		t.Fatal("shutdown func must be non-nil even when tracing is disabled")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("no-op shutdown returned %v", err)
	}
}
