// Package spantest is the test helper for fleet's span lifecycle (#2052): it
// installs a recording tracer provider for one test and asserts that every
// span the test started was ended, with the status its outcome earns. Span
// tests use it so a span left open on some path (an early return, a panic)
// fails the test that exercises that path.
package spantest

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/ElcanoTek/fleet/internal/telemetry"
)

// Recorder records the spans one test starts.
type Recorder struct {
	*tracetest.SpanRecorder
}

// Install makes a recording provider the global one for the test and
// restores the previous provider at cleanup. Tests using it must not run in
// parallel with other tests that read the global provider.
func Install(t testing.TB) *Recorder {
	t.Helper()
	prev := otel.GetTracerProvider()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
	})
	return &Recorder{SpanRecorder: sr}
}

// InstallChecked is Install plus a cleanup that fails the test when a span it
// started was never ended (AssertAllEnded), so every span test also checks
// the lifecycle of every span its path started.
func InstallChecked(t testing.TB) *Recorder {
	t.Helper()
	r := Install(t)
	// Registered after Install's cleanup, so it runs first, while the
	// recorder still belongs to this test.
	t.Cleanup(func() { r.AssertAllEnded(t) })
	return r
}

// EndedNamed waits (up to 10s, for spans ended on another goroutine) for an ended
// span named name and returns the first one.
func (r *Recorder) EndedNamed(t testing.TB, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, s := range r.Ended() {
			if s.Name() == name {
				return s
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no ended %q span (started: %s)", name, names(r.Started()))
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// AssertAllEnded fails when a span the test started was never ended — the
// lifecycle gap a deferred End on every path rules out. It waits briefly for
// spans ended on another goroutine.
func (r *Recorder) AssertAllEnded(t testing.TB) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		open := openSpans(r.SpanRecorder)
		if len(open) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("spans started but never ended: %s", strings.Join(open, ", "))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// AssertStatus checks a span's status code and, for an error, its
// error.type class (any non-empty class when class is "").
func AssertStatus(t testing.TB, s sdktrace.ReadOnlySpan, code codes.Code, class string) {
	t.Helper()
	if s.Status().Code != code {
		t.Fatalf("%s: status %v, want %v", s.Name(), s.Status(), code)
	}
	if code != codes.Error {
		return
	}
	got := ""
	for _, a := range s.Attributes() {
		if a.Key == telemetry.AttrErrClass {
			got = a.Value.AsString()
		}
	}
	if got == "" || (class != "" && got != class) {
		t.Fatalf("%s: error.type %q, want %q", s.Name(), got, class)
	}
}

func openSpans(sr *tracetest.SpanRecorder) []string {
	ended := map[[2]string]int{}
	for _, s := range sr.Ended() {
		ended[[2]string{s.SpanContext().TraceID().String(), s.SpanContext().SpanID().String()}]++
	}
	var open []string
	for _, s := range sr.Started() {
		k := [2]string{s.SpanContext().TraceID().String(), s.SpanContext().SpanID().String()}
		if ended[k] > 0 {
			ended[k]--
			continue
		}
		open = append(open, s.Name())
	}
	return open
}

func names(spans []sdktrace.ReadWriteSpan) string {
	out := make([]string, 0, len(spans))
	for _, s := range spans {
		out = append(out, s.Name())
	}
	return strings.Join(out, ", ")
}
