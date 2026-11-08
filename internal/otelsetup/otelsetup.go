// Package otelsetup wires OpenTelemetry distributed tracing for the fleet
// process (#186) and its MCP broker child (#2052). It is deliberately optional
// and ZERO-OVERHEAD when disabled: if FLEET_OTEL_ENDPOINT is unset, Init
// installs no exporter and leaves the global no-op TracerProvider in place, so
// every span started deep in a handler is non-recording, allocation-light and
// does no network I/O. When the endpoint IS set, spans are batched to an
// OTLP/gRPC collector (the OpenTelemetry Collector, Tempo, Jaeger, Honeycomb).
//
// TRANSPORT SECURITY (#2052). Export is TLS by default, verified against the
// system roots or FLEET_OTEL_CA_FILE, optionally with a client certificate
// (FLEET_OTEL_CLIENT_CERT_FILE + FLEET_OTEL_CLIENT_KEY_FILE, i.e. mTLS).
// Plaintext needs FLEET_OTEL_INSECURE=true AND a loopback endpoint (a collector
// sidecar on the same host); a plaintext request for any other host is
// refused, because spans carry turn, task and user ids across the network.
//
// The W3C TraceContext propagator is installed in every case, so an inbound
// `traceparent` continues to thread request context even when this process is
// not itself exporting.
package otelsetup

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"google.golang.org/grpc/credentials"

	"github.com/ElcanoTek/fleet/internal/config/envreg"
	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/telemetry"
)

// tracerName is the instrumentation scope name used by this package's spans.
const tracerName = telemetry.TracerName

// Options names the process being traced.
type Options struct {
	// ServiceName is the service.name resource attribute. Empty means
	// FLEET_SERVICE_NAME (default "fleet").
	ServiceName string
	// ServiceVersion is the service.version resource attribute.
	ServiceVersion string
	// Process distinguishes fleet's processes under one service: "server" for
	// `fleet serve`, "mcp-broker" for the credential broker child. Recorded as
	// fleet.process.
	Process string
}

// ExporterConfig is the OTLP exporter's resolved configuration (from the
// registry; see ExporterConfigFromEnv).
type ExporterConfig struct {
	Endpoint       string // host:port, or a grpc/https/http URL
	Insecure       bool   // plaintext; loopback endpoints only
	CAFile         string // PEM bundle verifying the collector; empty = system roots
	ClientCertFile string // PEM client certificate for mTLS
	ClientKeyFile  string // PEM client key for mTLS
}

// ExporterConfigFromEnv reads the exporter settings from the registry.
func ExporterConfigFromEnv() ExporterConfig {
	insecure, _ := strconv.ParseBool(strings.TrimSpace(envreg.Get("FLEET_OTEL_INSECURE")))
	return ExporterConfig{
		Endpoint:       strings.TrimSpace(envreg.Get("FLEET_OTEL_ENDPOINT")),
		Insecure:       insecure,
		CAFile:         strings.TrimSpace(envreg.Get("FLEET_OTEL_CA_FILE")),
		ClientCertFile: strings.TrimSpace(envreg.Get("FLEET_OTEL_CLIENT_CERT_FILE")),
		ClientKeyFile:  strings.TrimSpace(envreg.Get("FLEET_OTEL_CLIENT_KEY_FILE")),
	}
}

// Init configures the global OTel tracer and text-map propagator for the
// fleet server (Options{Process: "server"}). See InitWith.
func Init(ctx context.Context, serviceVersion string) (shutdown func(context.Context) error, err error) {
	return InitWith(ctx, Options{ServiceVersion: serviceVersion, Process: "server"})
}

// InitWith configures the global OTel tracer and text-map propagator.
//
//   - FLEET_OTEL_ENDPOINT unset → no exporter; the global no-op provider stays,
//     so instrumentation runs with zero export overhead. Returns a no-op shutdown.
//   - FLEET_OTEL_ENDPOINT set   → an OTLP/gRPC batch exporter to it, over TLS
//     (or mTLS) unless FLEET_OTEL_INSECURE=true for a loopback endpoint. A
//     configuration error (unreadable CA, half a client key pair, plaintext to
//     a remote host) is returned and nothing is installed.
//
// FLEET_OTEL_SAMPLE_RATIO (a float in [0,1], default 1.0) sets a parent-based
// ratio sampler; 1.0 = always sample. The returned shutdown MUST be deferred so
// buffered spans are flushed before the process exits.
func InitWith(ctx context.Context, opts Options) (shutdown func(context.Context) error, err error) {
	// Always install W3C TraceContext + Baggage propagation — free, and lets a
	// caller's trace span continue across this process even when export is off.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	cfg := ExporterConfigFromEnv()
	if cfg.Endpoint == "" {
		// Disabled: leave the global no-op TracerProvider in place.
		return func(context.Context) error { return nil }, nil
	}
	exporterOpts, err := cfg.grpcOptions()
	if err != nil {
		return nil, err
	}
	exp, err := otlptracegrpc.New(ctx, exporterOpts...)
	if err != nil {
		return nil, fmt.Errorf("otel exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(newResource(opts)),
		sdktrace.WithSampler(samplerFromEnv()),
	)
	otel.SetTracerProvider(tp)
	// Export failures (collector down, TLS refused) surface as structured
	// warnings instead of the SDK's default stdlib-log line.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logging.From(ctx).Warn("otel: export error", "err", err)
	}))
	logging.From(ctx).Info("otel: tracing enabled", "endpoint", cfg.Endpoint, "transport", cfg.transport(), "process", opts.Process)
	return tp.Shutdown, nil
}

// transport names the configured transport for the boot line.
func (c ExporterConfig) transport() string {
	switch {
	case c.Insecure:
		return "plaintext (loopback)"
	case c.ClientCertFile != "":
		return "mtls"
	default:
		return "tls"
	}
}

// grpcOptions resolves the endpoint and transport credentials.
func (c ExporterConfig) grpcOptions() ([]otlptracegrpc.Option, error) {
	host, hostport, err := splitEndpoint(c.Endpoint)
	if err != nil {
		return nil, err
	}
	opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(hostport)}
	if c.Insecure {
		if !isLoopbackHost(host) {
			return nil, fmt.Errorf("otel: FLEET_OTEL_INSECURE=true is allowed only for a loopback endpoint (got host %q); configure TLS (FLEET_OTEL_CA_FILE, optionally FLEET_OTEL_CLIENT_CERT_FILE/KEY_FILE) for a remote collector", host)
		}
		if c.CAFile != "" || c.ClientCertFile != "" || c.ClientKeyFile != "" {
			return nil, errors.New("otel: FLEET_OTEL_INSECURE=true contradicts the TLS settings (FLEET_OTEL_CA_FILE / FLEET_OTEL_CLIENT_*); set one or the other")
		}
		return append(opts, otlptracegrpc.WithInsecure()), nil
	}
	tlsCfg, err := c.tlsConfig(host)
	if err != nil {
		return nil, err
	}
	return append(opts, otlptracegrpc.WithTLSCredentials(credentials.NewTLS(tlsCfg))), nil
}

// tlsConfig builds the client TLS config: TLS 1.2+, the collector verified
// against FLEET_OTEL_CA_FILE (or the system roots), and a client certificate
// when both halves of the pair are set.
func (c ExporterConfig) tlsConfig(serverName string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("otel: read FLEET_OTEL_CA_FILE: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("otel: FLEET_OTEL_CA_FILE %s holds no PEM certificate", c.CAFile)
		}
		cfg.RootCAs = pool
	}
	switch {
	case c.ClientCertFile != "" && c.ClientKeyFile != "":
		pair, err := tls.LoadX509KeyPair(c.ClientCertFile, c.ClientKeyFile)
		if err != nil {
			return nil, fmt.Errorf("otel: load the mTLS client certificate: %w", err)
		}
		cfg.Certificates = []tls.Certificate{pair}
	case c.ClientCertFile != "" || c.ClientKeyFile != "":
		return nil, errors.New("otel: mTLS needs both FLEET_OTEL_CLIENT_CERT_FILE and FLEET_OTEL_CLIENT_KEY_FILE")
	}
	return cfg, nil
}

// splitEndpoint accepts "host:port" or a URL ("https://collector:4317",
// "grpc://…") and returns the host and the host:port the gRPC client dials.
// A URL's scheme does not choose the transport — FLEET_OTEL_INSECURE does — so
// an "http://" endpoint is not silently plaintext.
func splitEndpoint(endpoint string) (host, hostport string, err error) {
	hostport = endpoint
	if strings.Contains(endpoint, "://") {
		u, perr := url.Parse(endpoint)
		if perr != nil || u.Host == "" {
			return "", "", fmt.Errorf("otel: FLEET_OTEL_ENDPOINT %q is not host:port or a URL with a host", endpoint)
		}
		hostport = u.Host
	}
	h, port, serr := net.SplitHostPort(hostport)
	if serr != nil {
		return "", "", fmt.Errorf("otel: FLEET_OTEL_ENDPOINT %q needs a port (host:port): %w", endpoint, serr)
	}
	if h == "" || port == "" {
		return "", "", fmt.Errorf("otel: FLEET_OTEL_ENDPOINT %q needs a host and a port", endpoint)
	}
	return h, hostport, nil
}

// isLoopbackHost reports whether host names this machine: "localhost" or a
// loopback IP literal. A hostname is not resolved — DNS could point anywhere.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// newResource describes this service for exported spans. It builds our
// attributes as a SCHEMALESS resource and merges them over the SDK default
// resource (host/SDK attributes). Schemaless is deliberate: resource.Merge
// returns ErrSchemaURLConflict when two NON-empty schema URLs differ, and the
// SDK default carries whatever semconv version the SDK was built against — so
// pinning our own semconv schema URL would make the merge always fail and drop
// service.name/service.version. With an empty schema URL there is no conflict
// regardless of the SDK's version. On any unexpected merge error we still return
// OUR attributes (never an attribute-less default), so service.name and the
// version are always present on exported spans.
func newResource(opts Options) *resource.Resource {
	name := strings.TrimSpace(opts.ServiceName)
	if name == "" {
		name = strings.TrimSpace(envreg.Get("FLEET_SERVICE_NAME"))
	}
	if name == "" {
		name = "fleet"
	}
	kvs := []attribute.KeyValue{
		semconv.ServiceName(name),
		semconv.ServiceVersion(opts.ServiceVersion),
	}
	if opts.Process != "" {
		kvs = append(kvs, attribute.String("fleet.process", opts.Process))
	}
	attrs := resource.NewSchemaless(kvs...)
	merged, err := resource.Merge(resource.Default(), attrs)
	if err != nil {
		return attrs
	}
	return merged
}

// samplerFromEnv builds a parent-based sampler from FLEET_OTEL_SAMPLE_RATIO.
// Unset / unparseable / ≥1 → AlwaysSample; ≤0 → never (root) sample; otherwise a
// TraceIDRatioBased head sampler. Parent-based in all cases so a sampled inbound
// trace is always continued (we never drop a child of an already-sampled trace)
// — which is also what keeps a delegated broker call in its turn's trace: the
// broker child follows the parent's sampling decision carried on traceparent.
func samplerFromEnv() sdktrace.Sampler {
	raw := strings.TrimSpace(envreg.Get("FLEET_OTEL_SAMPLE_RATIO"))
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
