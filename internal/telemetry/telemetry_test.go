package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func TestErrorClassNamesTheTypeNotTheMessage(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nil, ""},
		{context.Canceled, "canceled"},
		{fmt.Errorf("wrapped: %w", context.DeadlineExceeded), "deadline_exceeded"},
		{&url.Error{Op: "Get", URL: "https://user:sk-secret@host", Err: errors.New("boom")}, "*url.Error"},
		{errors.New("Bearer sk-ant-secret"), "*errors.errorString"},
	} {
		if got := ErrorClass(tc.err); got != tc.want {
			t.Errorf("ErrorClass(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

func TestContextWithRemoteParentKeepsTraceNotCancellation(t *testing.T) {
	tid, _ := trace.TraceIDFromHex("0af7651916cd43dd8448eb211c80319c")
	sid, _ := trace.SpanIDFromHex("b7ad6b7169203331")
	parent, cancel := context.WithCancel(trace.ContextWithSpanContext(context.Background(),
		trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled})))
	cancel()
	ctx := ContextWithRemoteParent(context.Background(), parent)
	if ctx.Err() != nil {
		t.Error("the detached context inherited the parent's cancellation")
	}
	if TraceID(ctx) != tid.String() {
		t.Errorf("TraceID = %q, want the parent's", TraceID(ctx))
	}
	if TraceID(context.Background()) != "" {
		t.Error("a context without a span has no trace id")
	}
}
