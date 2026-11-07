package mcpbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/ElcanoTek/fleet/internal/logging"
)

// traceEchoBackend answers every call with what its call context carries on
// the CHILD side of the boundary: the trace id, the id of the span the child's
// serve span was parented to, and the correlation ids. The parent then checks
// them against its own spans — so the assertion is about what crossed the
// pipe, not about shared process state.
type traceEchoBackend struct{ *fakeBroker }

func (traceEchoBackend) CallMCP(ctx context.Context, _, _ string, _ map[string]any) (string, bool, error) {
	span := trace.SpanFromContext(ctx)
	parent := ""
	if ro, ok := span.(sdktrace.ReadOnlySpan); ok {
		parent = ro.Parent().SpanID().String()
	}
	return fmt.Sprintf("%s %s %s %s",
		span.SpanContext().TraceID(), parent,
		logging.Value(ctx, logging.KeyTurnID), logging.Value(ctx, logging.KeyConvID)), false, nil
}

// serveTraceChild is the child half of TestBrokerTracePropagation_RealSubprocess:
// a separate process with its OWN recording TracerProvider, which shares
// nothing with the parent but the stdio frames.
func serveTraceChild() {
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	otel.SetTracerProvider(tp)
	_ = ServeStdio(context.Background(), traceEchoBackend{&fakeBroker{}})
}

// withSpanRecorder installs a recording provider for the test (global state:
// these tests do not run in parallel).
func withSpanRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	prev := otel.GetTracerProvider()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
	})
	return sr
}

// A delegated MCP call lands in the caller's trace across a REAL process
// boundary: the child process continues the parent's mcpbroker.call span from
// the traceparent on the request frame, and the turn's correlation ids ride
// along for its log lines.
func TestBrokerTracePropagation_RealSubprocess(t *testing.T) {
	sr := withSpanRecorder(t)
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "MCPBROKER_TEST_CHILD=trace")
	cmd.Stderr = os.Stderr
	client, stop, err := SpawnClient(cmd)
	if err != nil {
		t.Fatalf("SpawnClient: %v", err)
	}
	defer func() { _ = stop() }()

	ctx := logging.WithTurn(context.Background(), "conv-7", "turn-42")
	ctx, turn := otel.Tracer("test").Start(ctx, "chat.turn")
	text, _, err := client.CallMCP(ctx, "deal_sheet", "lookup", nil)
	turn.End()
	if err != nil {
		t.Fatalf("CallMCP: %v", err)
	}

	var callSpan sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == "mcpbroker.call" {
			callSpan = s
		}
	}
	if callSpan == nil {
		t.Fatal("the parent recorded no mcpbroker.call client span")
	}
	if callSpan.Parent().SpanID() != turn.SpanContext().SpanID() {
		t.Errorf("mcpbroker.call is not a child of the turn span")
	}
	want := fmt.Sprintf("%s %s turn-42 conv-7", turn.SpanContext().TraceID(), callSpan.SpanContext().SpanID())
	if text != want {
		t.Fatalf("child saw %q, want %q (same trace, parented to the client span, ids carried)", text, want)
	}
}

// frameTap records every byte the client writes to the server.
type frameTap struct {
	net.Conn
	mu  sync.Mutex
	buf bytes.Buffer
}

func (f *frameTap) Write(p []byte) (int, error) {
	f.mu.Lock()
	f.buf.Write(p)
	f.mu.Unlock()
	return f.Conn.Write(p)
}

// The frame carries the W3C traceparent and the correlation ids — public
// identifiers only — and an uncorrelated call carries neither field.
func TestRequestFrameCarriesTraceContextAndIDsOnly(t *testing.T) {
	withSpanRecorder(t)
	cConn, sConn := net.Pipe()
	tap := &frameTap{Conn: cConn}
	srv := NewServer(&fakeBroker{})
	sctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(sctx, sConn); close(done) }()
	client := NewClient(tap)
	t.Cleanup(func() { _ = client.Close(); cancel(); <-done })

	// Uncorrelated: no span, no ids → no trace/log fields on the wire. The
	// client's own span is recording here, so stop it from counting: use a
	// fresh no-op provider for this one call.
	otel.SetTracerProvider(noop.NewTracerProvider())
	if _, _, err := client.CallMCP(context.Background(), "s", "t", nil); err != nil {
		t.Fatal(err)
	}
	if got := tap.String(); strings.Contains(got, `"trace"`) || strings.Contains(got, `"log"`) {
		t.Fatalf("an uncorrelated frame carries correlation fields: %s", got)
	}
	withSpanRecorder(t)
	tap.Reset()

	ctx := logging.WithTask(logging.WithUser(context.Background(), "a@b.co"), "task-1", "run-1")
	ctx, span := otel.Tracer("test").Start(ctx, "task.run")
	defer span.End()
	if _, _, err := client.CallMCP(ctx, "s", "t", map[string]any{"q": "secret question"}); err != nil {
		t.Fatal(err)
	}
	var req request
	if err := json.NewDecoder(strings.NewReader(tap.String())).Decode(&req); err != nil {
		t.Fatal(err)
	}
	if tp := req.Trace["traceparent"]; !strings.Contains(tp, span.SpanContext().TraceID().String()) {
		t.Errorf("traceparent = %q, want the caller's trace id", tp)
	}
	wantLog := map[string]string{"task_id": "task-1", "run_id": "run-1", "user_id": "a@b.co"}
	if fmt.Sprint(req.Log) != fmt.Sprint(wantLog) {
		t.Errorf("log ids = %v, want %v", req.Log, wantLog)
	}
}

func (f *frameTap) String() string { f.mu.Lock(); defer f.mu.Unlock(); return f.buf.String() }
func (f *frameTap) Reset()         { f.mu.Lock(); f.buf.Reset(); f.mu.Unlock() }

// The child honors only the correlation keys it knows: a parent cannot inject
// arbitrary attributes into the credential owner's log.
func TestAdoptCorrelationDropsUnknownKeys(t *testing.T) {
	ctx := adoptCorrelation(context.Background(), request{Log: map[string]string{
		"turn_id": "t1", "api_key": "sk-should-never-be-adopted", "msg": "forged",
	}})
	attrs := logging.Attrs(ctx)
	if len(attrs) != 1 || attrs[0].Key != "turn_id" {
		t.Fatalf("adopted %v, want only turn_id", attrs)
	}
}

// A masked broker failure is logged with the caller's correlation ids and with
// the secret it quoted scrubbed.
func TestLogMaskedCarriesIDsAndRedacts(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewHandler("json", &buf, slog.LevelDebug)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	secret := "ghp_" + strings.Repeat("Z", 36)
	ctx := logging.WithTurn(context.Background(), "c1", "t1")
	logMasked(ctx, "tool call", "deals.lookup", fmt.Errorf("upstream 500: Authorization: Bearer %s", secret))
	out := buf.String()
	if strings.Contains(out, secret) {
		t.Fatalf("masked-error log leaked the token: %s", out)
	}
	var m map[string]any
	if err := json.NewDecoder(io.LimitReader(&buf, 1<<20)).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if m["turn_id"] != "t1" || m["conv_id"] != "c1" || m["op"] != "tool call" {
		t.Errorf("record = %v, want the turn's ids and the op", m)
	}
}
