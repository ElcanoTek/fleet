package tools

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
)

// runToolCtx runs a tool with a caller-supplied context (the shared callTool
// helper uses context.Background(); ask/notify need a ctx-installed handler).
func runToolCtx(ctx context.Context, t *testing.T, tool fantasy.AgentTool, args string) fantasy.ToolResponse {
	t.Helper()
	resp, err := tool.Run(ctx, fantasy.ToolCall{Input: args})
	if err != nil {
		t.Fatalf("tool %s returned a Go error: %v", tool.Info().Name, err)
	}
	return resp
}

func TestNotifyTool(t *testing.T) {
	var got string
	ctx := WithNotifyHandler(context.Background(), func(m string) { got = m })
	resp := runToolCtx(ctx, t, NewNotifyTool(), `{"message":"stage 1 done"}`)
	if got != "stage 1 done" {
		t.Fatalf("handler not invoked: %q", got)
	}
	if resp.IsError {
		t.Fatalf("notify should succeed: %+v", resp)
	}
	// No handler → no-op success (the run continues).
	if r := runToolCtx(context.Background(), t, NewNotifyTool(), `{"message":"x"}`); r.IsError {
		t.Fatalf("notify without handler must not error: %+v", r)
	}
	// Empty message errors.
	if r := runToolCtx(ctx, t, NewNotifyTool(), `{"message":""}`); !r.IsError {
		t.Fatal("empty message must error")
	}
}

func TestAskTool(t *testing.T) {
	var asked string
	ctx := WithAskHandler(context.Background(), func(q string) error { asked = q; return nil })
	resp := runToolCtx(ctx, t, NewAskTool(), `{"question":"which region?"}`)
	if asked != "which region?" {
		t.Fatalf("handler not invoked: %q", asked)
	}
	if resp.IsError || !strings.Contains(resp.Content, "pausing") {
		t.Fatalf("ask success response: %+v", resp)
	}
	// No handler → clear ASK_UNAVAILABLE error, never a silent hang.
	r := runToolCtx(context.Background(), t, NewAskTool(), `{"question":"x"}`)
	if !r.IsError || !strings.Contains(r.Content, "ASK_UNAVAILABLE") {
		t.Fatalf("ask without handler must error clearly: %+v", r)
	}
	// installed predicates
	if !AskHandlerInstalled(ctx) || !NotifyHandlerInstalled(WithNotifyHandler(context.Background(), func(string) {})) {
		t.Fatal("installed predicates")
	}
	if AskHandlerInstalled(context.Background()) {
		t.Fatal("no handler → not installed")
	}
}
