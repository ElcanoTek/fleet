// Package otelsetup wires OpenTelemetry distributed tracing for the fleet
// process (#186). It is deliberately optional and ZERO-OVERHEAD when disabled:
// if FLEET_OTEL_ENDPOINT is unset, Init installs no exporter and leaves the
// global no-op TracerProvider in place, so every otel.Tracer(...).Start(...)
// call deep in a handler returns a non-recording span with no allocations of
// consequence and no network I/O. When the endpoint IS set, spans are batched
// to an OTLP/gRPC collector (Jaeger / Tempo / Honeycomb / the OTel Collector).
//
// The W3C TraceContext propagator is installed in BOTH cases, so an inbound
// `traceparent` continues to thread request context even when this process is
// not itself exporting.
package otelsetup

import (
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
)

// tracerName is the instrumentation scope name used by this package's spans.
const tracerName = "github.com/ElcanoTek/fleet"

// Init configures the global OTel tracer and text-map propagator.
//
//   - FLEET_OTEL_ENDPOINT unset  → no exporter; the global no-op provider stays,
//     so instrumentation runs with zero export overhead. Returns a no-op shutdown.
//   - FLEET_OTEL_ENDPOINT set     → an OTLP/gRPC batch exporter to that host:port
//     (plaintext; TLS is a deliberate follow-up). serviceVersion is recorded as
//     the service.version resource attribute.
//
// FLEET_OTEL_SAMPLE_RATIO (a float in [0,1], default 1.0) sets a parent-based
// ratio sampler; 1.0 = always sample. The returned shutdown MUST be deferred so
// buffered spans are flushed before the process exits.
func Init(ctx context.Context, serviceVersion string) (shutdown func(context.Context) error, err error) {
	// Always install W3C TraceContext + Baggage propagation — free, and lets a
	// caller's trace span continue across this process even when export is off.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	endpoint := strings.TrimSpace(os.Getenv("FLEET_OTEL_ENDPOINT"))
	if endpoint == "" {
		// Disabled: leave the global no-op TracerProvider in place.
		return func(context.Context) error { return nil }, nil
	}

	exp, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("otel exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(newResource(serviceVersion)),
		sdktrace.WithSampler(samplerFromEnv()),
	)
	otel.SetTracerProvider(tp)
	//nolint:gosec // G706: endpoint is operator-set startup config, logged %q-quoted (CR/LF escaped) — not attacker-controlled request input.
	log.Printf("otel: tracing enabled → %q", endpoint)
	return tp.Shutdown, nil
}

// newResource describes this service for exported spans. It builds our
// attributes as a SCHEMALESS resource and merges them over the SDK default
// resource (host/SDK attributes). Schemaless is deliberate: resource.Merge
// returns ErrSchemaURLConflict when two NON-empty schema URLs differ, and the
// SDK default carries whatever semconv version the SDK was built against — so
// pinning our own semconv schema URL would make the merge always fail and drop
// service.name/service.version. With an empty schema URL there is no conflict
// regardless of the SDK's version. On any unexpected merge error we still return
// OUR attributes (never an attribute-less default), so service.name=fleet and
// the version are always present on exported spans.
func newResource(serviceVersion string) *resource.Resource {
	attrs := resource.NewSchemaless(
		semconv.ServiceName("fleet"),
		semconv.ServiceVersion(serviceVersion),
	)
	merged, err := resource.Merge(resource.Default(), attrs)
	if err != nil {
		return attrs
	}
	return merged
}

// samplerFromEnv builds a parent-based sampler from FLEET_OTEL_SAMPLE_RATIO.
// Unset / unparseable / ≥1 → AlwaysSample; ≤0 → never (root) sample; otherwise a
// TraceIDRatioBased head sampler. Parent-based in all cases so a sampled inbound
// trace is always continued (we never drop a child of an already-sampled trace).
func samplerFromEnv() sdktrace.Sampler {
	raw := strings.TrimSpace(os.Getenv("FLEET_OTEL_SAMPLE_RATIO"))
	if raw == "" {
		return sdktrace.ParentBased(sdktrace.AlwaysSample())
	}
	ratio, err := strconv.ParseFloat(raw, 64)
	// NaN/±Inf parse without error but would silently drop all root traces, so
	// treat any non-finite or ≥1 value as "sample everything" (the documented
	// default), and ≤0 as "never".
	if err != nil || math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio >= 1 {
		return sdktrace.ParentBased(sdktrace.AlwaysSample())
	}
	if ratio <= 0 {
		return sdktrace.ParentBased(sdktrace.NeverSample())
	}
	return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))
}
