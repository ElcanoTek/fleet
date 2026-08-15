package safe

import (
	"bytes"
	"log/slog"
	"regexp"
	"strings"
	"testing"
)

func TestEmitPanic_CountsAndFansOut(t *testing.T) {
	const loc = "unit.emit-test"

	var hookName string
	var hookValue any
	var hookStack []byte
	var writerLoc, writerMsg string
	prevSentry, prevWriter := SentryHook, PanicEventWriter
	SentryHook = func(name string, value any, stack []byte) {
		hookName, hookValue, hookStack = name, value, stack
	}
	PanicEventWriter = func(location, message string, _ []byte) { writerLoc, writerMsg = location, message }
	t.Cleanup(func() { SentryHook, PanicEventWriter = prevSentry, prevWriter })

	before := PanicCounts()[loc]
	EmitPanic(loc, "kaboom", []byte("stack"))

	if got := PanicCounts()[loc]; got != before+1 {
		t.Errorf("PanicCounts[%s] = %d, want %d", loc, got, before+1)
	}
	if hookName != loc {
		t.Errorf("SentryHook got %q, want %q", hookName, loc)
	}
	if hookValue != "string" || hookStack != nil {
		t.Errorf("SentryHook diagnostics = (%v,%q), want sanitized class and nil stack", hookValue, hookStack)
	}
	if writerLoc != loc || writerMsg != "string" {
		t.Errorf("PanicEventWriter got (%q,%q), want (%q,string)", writerLoc, writerMsg, loc)
	}
}

func TestRecover_FansOutToWriter(t *testing.T) {
	var gotMsg string
	prev := PanicEventWriter
	PanicEventWriter = func(_, message string, _ []byte) { gotMsg = message }
	t.Cleanup(func() { PanicEventWriter = prev })

	func() {
		defer Recover("unit.recover-writer", nil)
		panic("from-recover")
	}()
	if gotMsg != "string" {
		t.Errorf("writer message = %q, want sanitized class string", gotMsg)
	}
}

func TestRecover_RecoversAndRunsOnPanic(t *testing.T) {
	called := false
	var recovered any
	func() {
		defer Recover("unit", func(v any) {
			called = true
			recovered = v
		})
		panic("boom")
	}()
	if !called {
		t.Fatal("onPanic was not invoked after a panic")
	}
	if recovered != "boom" {
		t.Fatalf("onPanic received %v, want \"boom\"", recovered)
	}
}

func TestGo_RecoversPanic(t *testing.T) {
	previous := PanicEventHook
	recovered := make(chan struct{})
	PanicEventHook = func(event PanicEvent, _ any) {
		if event.Location == "unit.go" {
			close(recovered)
		}
	}
	t.Cleanup(func() { PanicEventHook = previous })

	// If Go did not recover, this panic would crash the whole test process.
	done := goWithDone("unit.go", func() { panic("boom") })
	<-recovered
	<-done
}

func TestEmitPanicWithMetadata_StableOpaqueIncidentAndAttribution(t *testing.T) {
	prevHook := PanicEventHook
	var hooked PanicEvent
	PanicEventHook = func(event PanicEvent, _ any) { hooked = event }
	t.Cleanup(func() { PanicEventHook = prevHook })

	meta := PanicMetadata{
		Location:       "agentcore.tool",
		Boundary:       "policy.record_tool_result",
		ToolName:       "mcp_crm_create",
		ToolCallID:     "call-7",
		RunMode:        "scheduled",
		TaskID:         "task-opaque",
		ConversationID: "conversation-opaque",
	}
	event := EmitPanicWithMetadata(meta, "operator-only panic", []byte("stack"))
	if !regexp.MustCompile(`^inc_[0-9a-f]{32}$`).MatchString(event.IncidentID) {
		t.Fatalf("incident ID %q is not opaque/stable format", event.IncidentID)
	}
	if hooked.IncidentID != event.IncidentID {
		t.Fatalf("hook incident = %q, returned event = %q", hooked.IncidentID, event.IncidentID)
	}
	if event.Class != "string" || hooked.Class != "string" {
		t.Fatalf("panic class = returned %q hooked %q, want string", event.Class, hooked.Class)
	}
	if hooked.ToolName != meta.ToolName || hooked.ToolCallID != meta.ToolCallID ||
		hooked.RunMode != meta.RunMode || hooked.TaskID != meta.TaskID ||
		hooked.ConversationID != meta.ConversationID || hooked.Boundary != meta.Boundary {
		t.Fatalf("hook attribution = %+v, want %+v", hooked.PanicMetadata, meta)
	}
	stat := PanicStats()[meta.Location]
	if stat.Count == 0 || stat.Last.IncidentID != event.IncidentID || stat.Last.ToolName != meta.ToolName {
		t.Fatalf("PanicStats[%q] = %+v, want latest attributed incident", meta.Location, stat)
	}
}

func TestRecoveredPanicSecretNeverCrossesTelemetrySeams(t *testing.T) {
	const secret = "Authorization: Bearer fake-regression-secret"

	previousLogger := panicLogger
	previousSentry := SentryHook
	previousEventHook := PanicEventHook
	previousWriter := PanicEventWriter
	previousStructured := StructuredPanicEventWriter
	t.Cleanup(func() {
		panicLogger = previousLogger
		SentryHook = previousSentry
		PanicEventHook = previousEventHook
		PanicEventWriter = previousWriter
		StructuredPanicEventWriter = previousStructured
	})

	var logs bytes.Buffer
	panicLogger = slog.New(slog.NewJSONHandler(&logs, nil))
	var sentryValue, eventValue any
	var sentryStack, writerStack []byte
	var writerMessage string
	var structured PanicEvent
	SentryHook = func(_ string, value any, stack []byte) { sentryValue, sentryStack = value, stack }
	PanicEventHook = func(event PanicEvent, value any) { structured, eventValue = event, value }
	PanicEventWriter = func(_ string, message string, stack []byte) { writerMessage, writerStack = message, stack }
	StructuredPanicEventWriter = func(event PanicEvent) { structured = event }

	func() {
		defer Recover("unit.secret-regression", nil)
		panic(secret)
	}()

	if sentryValue != "string" || eventValue != "string" || writerMessage != "string" || structured.Class != "string" {
		t.Fatalf("telemetry did not receive only the sanitized class: sentry=%v event=%v writer=%q structured=%+v",
			sentryValue, eventValue, writerMessage, structured)
	}
	if sentryStack != nil || writerStack != nil {
		t.Fatalf("telemetry received a stack: sentry=%q writer=%q", sentryStack, writerStack)
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatalf("structured recovery log leaked Authorization secret: %s", logs.String())
	}
}

func TestEmitPanicWithMetadata_ReportingHookPanicIsContained(t *testing.T) {
	prevHook := PanicEventHook
	PanicEventHook = func(PanicEvent, any) { panic("broken reporter") }
	t.Cleanup(func() { PanicEventHook = prevHook })

	// If reporting-hook containment regresses, this test process panics here.
	event := EmitPanicWithMetadata(PanicMetadata{Location: "unit.hook"}, "original", nil)
	if event.IncidentID == "" {
		t.Fatal("recovery event lost after reporting hook panic")
	}
}

// panicSecretError is an error whose Error() text is a credential — the exact
// shape PanicClass must never invoke.
type panicSecretError struct{ secret string }

func (e panicSecretError) Error() string { return e.secret }

// TestPanicClassNeverFormatsRecoveredValue pins the value-free classification
// contract on the LIVE classifier: PanicClass must report only the recovered
// value's kind, never call Error()/String() on it, and never echo any part of
// it. A regression here would let a credential copied into a panic reach logs
// and telemetry, since every recovery boundary in the tree classifies through
// this function before anything is emitted.
func TestPanicClassNeverFormatsRecoveredValue(t *testing.T) {
	const secret = "Authorization: Bearer fake-panic-regression-secret"
	for _, tc := range []struct {
		name string
		val  any
		want string
	}{
		{"error carrying a secret", panicSecretError{secret: secret}, "error"},
		{"raw secret string", secret, "string"},
		{"nil", nil, "nil"},
		{"struct", struct{ Token string }{Token: secret}, "struct"},
	} {
		got := PanicClass(tc.val)
		if got != tc.want {
			t.Errorf("%s: PanicClass = %q, want %q", tc.name, got, tc.want)
		}
		if strings.Contains(got, secret) {
			t.Errorf("%s: PanicClass leaked the recovered value: %q", tc.name, got)
		}
	}
}
