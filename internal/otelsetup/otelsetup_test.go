package otelsetup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"
)

// withRecorder installs a recording TracerProvider + TraceContext propagator and
// returns the recorder. Tests using it must not run in parallel (global state).
func withRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return sr
}

func TestMiddlewareChiRoutePatternAndRequestID(t *testing.T) {
	sr := withRecorder(t)

	r := chi.NewRouter()
	r.Use(Middleware)
	r.Get("/tasks/{task_id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/tasks/abc123", nil))

	reqID := rec.Header().Get("X-Request-Id")
	if len(reqID) != 32 {
		t.Fatalf("X-Request-Id = %q, want 32 hex chars", reqID)
	}
	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(spans))
	}
	if got := spans[0].Name(); got != "GET /tasks/{task_id}" {
		t.Errorf("span name = %q, want low-cardinality route pattern", got)
	}
	// X-Request-Id should equal the span's trace id when tracing is recording.
	if spans[0].SpanContext().TraceID().String() != reqID {
		t.Errorf("X-Request-Id %q != trace id %s", reqID, spans[0].SpanContext().TraceID())
	}
	var sawStatus, sawMethod bool
	for _, a := range spans[0].Attributes() {
		switch string(a.Key) {
		case "http.response.status_code":
			sawStatus = a.Value.AsInt64() == 200
		case "http.request.method":
			sawMethod = a.Value.AsString() == "GET"
		}
	}
	if !sawStatus || !sawMethod {
		t.Errorf("missing expected attributes (status=%v method=%v)", sawStatus, sawMethod)
	}
}

func TestMiddlewareServeMuxPattern(t *testing.T) {
	sr := withRecorder(t)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /conversations/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	h := Middleware(mux)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/conversations/xyz", nil))

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(spans))
	}
	// Validates the stdlib ServeMux r.Pattern path of routePattern.
	if got := spans[0].Name(); got != "GET /conversations/{id}" {
		t.Errorf("span name = %q, want ServeMux pattern", got)
	}
}

func TestMiddlewareSkipsHealthPaths(t *testing.T) {
	sr := withRecorder(t)

	called := false
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if !called {
		t.Fatal("health request should still reach the handler")
	}
	if n := len(sr.Ended()); n != 0 {
		t.Errorf("health path produced %d spans, want 0 (suppressed)", n)
	}
	if rec.Header().Get("X-Request-Id") != "" {
		t.Error("health path should not set X-Request-Id (fully skipped)")
	}
}

func TestMiddlewarePropagatesInboundTrace(t *testing.T) {
	sr := withRecorder(t)

	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	const traceID = "0af7651916cd43dd8448eb211c80319c"
	req.Header.Set("traceparent", "00-"+traceID+"-b7ad6b7169203331-01")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(spans))
	}
	if got := spans[0].SpanContext().TraceID().String(); got != traceID {
		t.Errorf("server span trace id = %s, want inherited %s", got, traceID)
	}
	if rec.Header().Get("X-Request-Id") != traceID {
		t.Errorf("X-Request-Id = %q, want inherited trace id %s", rec.Header().Get("X-Request-Id"), traceID)
	}
}

func TestMiddlewareRandomRequestIDWhenDisabled(t *testing.T) {
	// No recording provider → the no-op tracer yields a zero trace id, so the
	// middleware must mint a random X-Request-Id.
	otel.SetTracerProvider(noop.NewTracerProvider())
	otel.SetTextMapPropagator(propagation.TraceContext{})

	h := Middleware(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	reqID := rec.Header().Get("X-Request-Id")
	if len(reqID) != 32 {
		t.Fatalf("X-Request-Id = %q, want a 32-hex random id when tracing disabled", reqID)
	}
}

func TestMiddlewarePreservesFlusher(t *testing.T) {
	withRecorder(t)
	// SSE endpoints (/chat, /tasks/{id}/stream) need http.Flusher to survive the
	// response-writer wrap, or streaming silently buffers.
	var gotFlusher bool
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, gotFlusher = w.(http.Flusher)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/stream", nil))
	if !gotFlusher {
		t.Fatal("handler must still receive an http.Flusher after the tracing wrap (SSE)")
	}
}

func TestInitNoopWhenEndpointUnset(t *testing.T) {
	t.Setenv("FLEET_OTEL_ENDPOINT", "")
	shutdown, err := Init(context.Background(), "test-version")
	if err != nil {
		t.Fatalf("Init returned error with no endpoint: %v", err)
	}
	if shutdown == nil {
		t.Fatal("Init returned nil shutdown")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("no-op shutdown returned error: %v", err)
	}
	// The W3C propagator must still be installed so inbound context flows.
	fields := otel.GetTextMapPropagator().Fields()
	if !contains(fields, "traceparent") {
		t.Errorf("propagator fields = %v, want traceparent", fields)
	}
}

func TestNewResourceCarriesServiceIdentity(t *testing.T) {
	// Regression guard: a semconv/SDK schema-URL mismatch must NOT drop our
	// service identity (the schemaless merge keeps service.name + version).
	res := newResource("1.2.3")
	var name, ver string
	for _, kv := range res.Attributes() {
		switch string(kv.Key) {
		case "service.name":
			name = kv.Value.AsString()
		case "service.version":
			ver = kv.Value.AsString()
		}
	}
	if name != "fleet" {
		t.Errorf("service.name = %q, want fleet", name)
	}
	if ver != "1.2.3" {
		t.Errorf("service.version = %q, want 1.2.3", ver)
	}
}

func TestSamplerFromEnv(t *testing.T) {
	cases := []struct {
		val      string
		contains string
	}{
		{"", "AlwaysOnSampler"},
		{"1.0", "AlwaysOnSampler"},
		{"2", "AlwaysOnSampler"},
		{"abc", "AlwaysOnSampler"},
		{"0", "AlwaysOffSampler"},
		{"-1", "AlwaysOffSampler"},
		{"0.25", "TraceIDRatioBased"},
		{"NaN", "AlwaysOnSampler"},
		{"Inf", "AlwaysOnSampler"},
		{"-Inf", "AlwaysOnSampler"},
	}
	for _, c := range cases {
		t.Run(c.val, func(t *testing.T) {
			t.Setenv("FLEET_OTEL_SAMPLE_RATIO", c.val)
			desc := samplerFromEnv().Description()
			if !strings.Contains(desc, c.contains) {
				t.Errorf("ratio %q → sampler %q, want it to contain %q", c.val, desc, c.contains)
			}
		})
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
