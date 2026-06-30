package otelsetup

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// healthPaths are low-value, high-frequency probe endpoints excluded from
// tracing so they don't drown a collector in noise. Span suppression here costs
// nothing when tracing is disabled (the span would be non-recording anyway) and
// keeps the trace stream meaningful when it is enabled.
var healthPaths = map[string]struct{}{
	"/health":  {},
	"/healthz": {},
	"/livez":   {},
	"/readyz":  {},
	"/metrics": {},
}

// Middleware returns an HTTP middleware that, for every request:
//   - extracts any inbound W3C trace context (traceparent/baggage) so this
//     server span continues a caller's distributed trace;
//   - starts a SERVER span (non-recording and ~free when tracing is disabled);
//   - sets an X-Request-Id response header (the trace id when tracing is on,
//     else a fresh random id) so logs and responses are correlatable even with
//     export off — this was previously absent entirely;
//   - names the span by the matched ROUTE PATTERN (low cardinality) — chi's
//     RoutePattern for the orchestrator, the stdlib ServeMux r.Pattern for the
//     chat server — falling back to the method alone.
//
// It is router-agnostic: it works as a chi r.Use middleware AND as a top-level
// net/http wrapper. The response writer is wrapped with chi's
// WrapResponseWriter, which preserves http.Flusher/Hijacker so SSE streams are
// unaffected.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, skip := healthPaths[r.URL.Path]; skip {
			next.ServeHTTP(w, r)
			return
		}

		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := otel.Tracer(tracerName).Start(ctx, r.Method, trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()

		reqID := requestIDFromSpan(span)
		w.Header().Set("X-Request-Id", reqID)

		ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)
		// Pass the trace-context request DOWN; the router populates its
		// pattern on THIS request value, which we read back after it returns.
		r = r.WithContext(ctx)
		next.ServeHTTP(ww, r)

		status := ww.Status()
		if status == 0 {
			status = http.StatusOK // handler wrote a body without an explicit WriteHeader
		}
		// Name the span "<METHOD> <route>". chi's RoutePattern is path-only
		// ("/tasks/{id}"); the stdlib ServeMux's r.Pattern already includes the
		// method ("GET /conversations/{id}"), so don't prepend it twice.
		name := r.Method
		if route := routePattern(r); route != "" {
			if strings.HasPrefix(route, r.Method+" ") {
				name = route
			} else {
				name = r.Method + " " + route
			}
		}
		span.SetName(name)
		span.SetAttributes(
			attribute.String("http.request.method", r.Method),
			attribute.String("url.path", r.URL.Path),
			attribute.Int("http.response.status_code", status),
			attribute.String("fleet.request_id", reqID),
		)
		// 5xx marks the span as errored; 4xx is a client problem, not a server
		// fault, so it is left unset (OTel HTTP-semconv convention).
		if status >= 500 {
			span.SetStatus(codes.Error, http.StatusText(status))
		}
	})
}

// routePattern returns the low-cardinality matched route for r: chi's
// RoutePattern when the request went through a chi router, otherwise the stdlib
// ServeMux's matched Pattern. Empty when neither is available (the caller then
// falls back to the bare method).
func routePattern(r *http.Request) string {
	if rc := chi.RouteContext(r.Context()); rc != nil {
		if p := rc.RoutePattern(); p != "" {
			return p
		}
	}
	return r.Pattern
}

// requestIDFromSpan derives a request id from the span's trace id only when the
// span is actually being RECORDED by this process — i.e. there is a real
// recorded trace to correlate against. When tracing is disabled (no-op provider)
// or the request was sampled out, it mints a random 128-bit id. Gating on
// IsRecording (not merely HasTraceID) prevents echoing a client-supplied inbound
// traceparent as the X-Request-Id when this process is not tracing, so the
// header is always a server-anchored value.
func requestIDFromSpan(span trace.Span) string {
	if span.IsRecording() {
		if sc := span.SpanContext(); sc.HasTraceID() {
			return sc.TraceID().String()
		}
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}
