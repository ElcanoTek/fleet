package logging

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

// withDefault installs a JSON handler over buf as slog's default for the test.
func withDefault(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(newJSONHandler(buf, slog.LevelDebug)))
	t.Cleanup(func() { slog.SetDefault(prev) })
}

func TestFromCarriesCorrelationAttrs(t *testing.T) {
	var buf bytes.Buffer
	withDefault(t, &buf)

	ctx := WithRequestID(context.Background(), "req-1")
	ctx = WithUser(ctx, "a@b.co")
	ctx = WithTurn(ctx, "conv-1", "turn-1")
	ctx = WithTask(ctx, "task-1", "run-1")
	// A re-attach replaces rather than duplicates.
	ctx = WithTurn(ctx, "conv-1", "turn-2")

	tid, _ := trace.TraceIDFromHex("0af7651916cd43dd8448eb211c80319c")
	sid, _ := trace.SpanIDFromHex("b7ad6b7169203331")
	ctx = trace.ContextWithSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled,
	}))

	From(ctx).Info("turn finished", "steps", 3)
	m := decodeLast(t, &buf)
	want := map[string]any{
		KeyRequestID: "req-1",
		KeyUserID:    "a@b.co",
		KeyConvID:    "conv-1",
		KeyTurnID:    "turn-2",
		KeyTaskID:    "task-1",
		KeyRunID:     "run-1",
		KeyTraceID:   tid.String(),
		KeySpanID:    sid.String(),
		"steps":      float64(3),
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %v, want %v", k, m[k], v)
		}
	}
	if strings.Count(buf.String(), `"turn_id"`) != 1 {
		t.Errorf("turn_id should appear once after a re-attach: %s", buf.String())
	}
	if got := Value(ctx, KeyTurnID); got != "turn-2" {
		t.Errorf("Value(turn_id) = %q", got)
	}
}

// A call site that names an id itself wins over the context's copy: one key,
// never a duplicate JSON field.
func TestFromCallSiteAttrWinsOverContext(t *testing.T) {
	var buf bytes.Buffer
	withDefault(t, &buf)
	ctx := WithTask(context.Background(), "ctx-task", "run-1")
	From(ctx).Info("acted on another task", "task_id", "other-task")
	if n := strings.Count(buf.String(), `"task_id"`); n != 1 {
		t.Fatalf("task_id appears %d times: %s", n, buf.String())
	}
	if m := decodeLast(t, &buf); m["task_id"] != "other-task" || m["run_id"] != "run-1" {
		t.Errorf("record = %v", m)
	}
	buf.Reset()
	From(ctx).With("task_id", "bound").Info("x")
	if n := strings.Count(buf.String(), `"task_id"`); n != 1 {
		t.Fatalf("With-bound task_id duplicated: %s", buf.String())
	}
}

func TestFromWithoutAttrsIsDefault(t *testing.T) {
	if From(context.Background()) != slog.Default() {
		t.Error("From on a bare context should return slog.Default() itself")
	}
	ctx := WithAttrs(context.Background(), slog.String(KeyConvID, ""))
	if len(Attrs(ctx)) != 0 {
		t.Error("an empty id must not be attached")
	}
}

func TestRedactsSecretShapedValues(t *testing.T) {
	var buf bytes.Buffer
	withDefault(t, &buf)

	secret := "sk-ant-" + strings.Repeat("A", 32)
	slog.Info("call failed: Authorization: Bearer abcdefghijklmnop",
		"err", errors.New("upstream said "+secret),
		"url", "https://x.example/?api_key=hunter2hunter2",
		"prompt_tokens", 1234,
		"session_token", "opaque-value",
	)
	out := buf.String()
	for _, leak := range []string{secret, "abcdefghijklmnop", "hunter2hunter2", "opaque-value"} {
		if strings.Contains(out, leak) {
			t.Errorf("log line leaked %q: %s", leak, out)
		}
	}
	m := decodeLast(t, &buf)
	if m["prompt_tokens"] != float64(1234) {
		t.Errorf("a numeric *_tokens count must not be redacted: %v", m["prompt_tokens"])
	}
	if m["session_token"] != "[REDACTED]" {
		t.Errorf("secret-keyed string not blanked: %v", m["session_token"])
	}
}

func TestRedactsRegisteredLiteral(t *testing.T) {
	var buf bytes.Buffer
	withDefault(t, &buf)
	// A bare credential of no recognizable shape is caught by value once the
	// broker/agentcore registration forwards it here.
	RegisterSecretLiterals("", false, "zq8Vv3n1Lw0pX7rT")
	From(WithConversation(context.Background(), "c1")).Warn("connector echoed", "detail", "got zq8Vv3n1Lw0pX7rT back")
	if strings.Contains(buf.String(), "zq8Vv3n1Lw0pX7rT") {
		t.Errorf("registered literal leaked: %s", buf.String())
	}
}

func TestRedactsWithBoundAttrs(t *testing.T) {
	var buf bytes.Buffer
	lg := slog.New(newTextHandler(&buf, slog.LevelInfo)).With("api_key", "abc", "note", "ghp_"+strings.Repeat("x", 36))
	lg.Info("hello")
	out := buf.String()
	if strings.Contains(out, "abc") || strings.Contains(out, "ghp_") {
		t.Errorf("With-bound attrs not redacted: %s", out)
	}
	if !strings.Contains(out, `msg=hello`) || !strings.Contains(out, "level=INFO") {
		t.Errorf("text format should be logfmt: %s", out)
	}
}
