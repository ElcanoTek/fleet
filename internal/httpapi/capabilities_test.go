package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestParseClientCapabilities(t *testing.T) {
	// Absent / malformed → nil (no filter), preserving backward compatibility.
	for _, hdr := range []string{"", "   ", "not json", "{}", `"text"`, "[1,2,3"} {
		if got := parseClientCapabilities(hdr); got != nil {
			t.Errorf("parseClientCapabilities(%q) = %v, want nil", hdr, got)
		}
	}

	// Valid JSON array → membership set; unknown tokens are retained (and simply
	// never match a governed event).
	got := parseClientCapabilities(`["text","tool_calls","future_unknown"]`)
	if got == nil {
		t.Fatal("expected a non-nil set")
	}
	if !got[CapText] || !got[CapToolCalls] {
		t.Errorf("missing declared caps: %v", got)
	}
	if got[CapReasoning] {
		t.Error("reasoning was not declared but is present")
	}
	if !got[SSECapability("future_unknown")] {
		t.Error("unknown token should be retained in the set")
	}

	// Empty array → non-nil empty set (an explicit "I handle no governed events"),
	// distinct from nil (no header).
	empty := parseClientCapabilities(`[]`)
	if empty == nil {
		t.Fatal("empty array should yield a non-nil (empty) set, not nil")
	}
	if len(empty) != 0 {
		t.Errorf("empty array should yield an empty set, got %v", empty)
	}
}

func TestShouldEmit(t *testing.T) {
	// nil caps = no filter: everything emits.
	for _, ev := range []string{"text.delta", "tool.call", "turn.completed", "anything"} {
		if !shouldEmit(nil, ev) {
			t.Errorf("nil caps should emit %q", ev)
		}
	}

	caps := map[SSECapability]bool{CapText: true} // text only

	// Governed + declared → emit; governed + undeclared → suppress.
	if !shouldEmit(caps, "text.delta") {
		t.Error("declared text.delta should emit")
	}
	for _, suppressed := range []string{"reasoning.delta", "tool.call", "tool.result", "tool.approval_required", "memory.proposed", "tool.auto_resolved"} {
		if shouldEmit(caps, suppressed) {
			t.Errorf("undeclared governed event %q should be suppressed", suppressed)
		}
	}

	// Lifecycle / control events are never governed → always emit, even with a
	// restrictive set.
	for _, lifecycle := range []string{"turn.started", "turn.completed", "turn.cancelled", "turn.error", "conversation", "conversation.title_updated", "user.message", "status", "reconnect", capabilitiesEventName} {
		if !shouldEmit(caps, lifecycle) {
			t.Errorf("lifecycle/control event %q must always emit", lifecycle)
		}
	}
}

func TestSupportedCapabilitiesJSON(t *testing.T) {
	var got []string
	if err := json.Unmarshal([]byte(supportedCapabilitiesJSON()), &got); err != nil {
		t.Fatalf("supported header is not valid JSON: %v", err)
	}
	want := map[string]bool{"text": true, "reasoning": true, "tool_calls": true, "tool_results": true, "approval_cards": true}
	if len(got) != len(want) {
		t.Errorf("supported set = %v, want keys %v", got, want)
	}
	for _, c := range got {
		if !want[c] {
			t.Errorf("unexpected capability %q advertised", c)
		}
	}
	// Honesty guard: events with no emit site in this repo must not be advertised.
	for _, absent := range []string{"enforcement_nudges", "usage_snapshots", "permissions"} {
		if strings.Contains(supportedCapabilitiesJSON(), absent) {
			t.Errorf("advertised %q, which has no emit site in this codebase", absent)
		}
	}
}

// TestAttach_FiltersByDeclaredCapabilities drives the real Attach replay path
// (deterministic on a sealed buffer) and asserts governed events the client did
// not declare are suppressed while lifecycle events and the synthetic
// capabilities frame always flow.
func TestAttach_FiltersByDeclaredCapabilities(t *testing.T) {
	buf := newTurnBuffer("conv-1", "turn-1")
	buf.Emit("turn.started", map[string]any{})
	buf.Emit("text.delta", map[string]any{"t": "hi"})
	buf.Emit("reasoning.delta", map[string]any{"t": "thinking"})
	buf.Emit("tool.call", map[string]any{"name": "bash"})
	buf.Emit("tool.result", map[string]any{"ok": true})
	buf.Emit("turn.completed", map[string]any{})
	buf.Finish()

	rw := newRecorder()
	caps := map[SSECapability]bool{CapText: true} // declares text only
	if err := buf.Attach(context.Background(), 0, rw, caps); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	body := rw.Body()

	// Supported-capabilities header + synthetic first frame are always present.
	if h := rw.Header().Get(supportedCapabilitiesHeaderName); h == "" || !strings.Contains(h, "text") {
		t.Errorf("X-Fleet-Supported-Capabilities header = %q", h)
	}
	if !strings.Contains(body, "event: "+capabilitiesEventName+"\n") {
		t.Errorf("missing fleet.capabilities frame:\n%s", body)
	}

	// Declared + lifecycle events present.
	for _, want := range []string{"event: turn.started\n", "event: text.delta\n", "event: turn.completed\n"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	// Undeclared governed events suppressed.
	for _, gone := range []string{"event: reasoning.delta\n", "event: tool.call\n", "event: tool.result\n"} {
		if strings.Contains(body, gone) {
			t.Errorf("suppressed event leaked: %q in:\n%s", gone, body)
		}
	}
}

// TestAttach_NilCapsEmitsEverything is the backward-compat guard: a client that
// sends no capability header gets the full stream, including governed events.
func TestAttach_NilCapsEmitsEverything(t *testing.T) {
	buf := newTurnBuffer("conv-1", "turn-1")
	buf.Emit("text.delta", map[string]any{"t": "hi"})
	buf.Emit("tool.call", map[string]any{"name": "bash"})
	buf.Emit("reasoning.delta", map[string]any{"t": "x"})
	buf.Finish()

	rw := newRecorder()
	if err := buf.Attach(context.Background(), 0, rw, nil); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	body := rw.Body()
	for _, want := range []string{"event: text.delta\n", "event: tool.call\n", "event: reasoning.delta\n"} {
		if !strings.Contains(body, want) {
			t.Errorf("nil caps should emit %q in:\n%s", want, body)
		}
	}
}
