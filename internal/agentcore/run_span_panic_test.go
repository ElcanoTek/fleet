package agentcore

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/codes"
)

// A panic the run backstop contains (recoverSynchronousRunPanic) turns into
// ErrRunBoundaryPanic; the agentcore.run span must end AFTER that recovery
// and record the failure, not a success with a nil error (#2052).
func TestRunSpanRecordsContainedPanic(t *testing.T) {
	_ = capturePanicEvents(t)
	sr := recordSpans(t)

	_, err := Run(context.Background(), ModeInteractive, RunConfig{ConversationID: "conv-span-panic"}, Deps{
		Input:  stubInput{system: "sys", user: "work", label: "span-panic"},
		Policy: passPolicy{},
		Model:  &mockModel{streamFunc: streamStop()},
		Finalize: func(context.Context, FinalizeInput) (string, error) {
			panic("finalize raw panic")
		},
	})
	if !errors.Is(err, ErrRunBoundaryPanic) {
		t.Fatalf("Run error = %v, want ErrRunBoundaryPanic", err)
	}
	run := spanNamed(t, sr, "agentcore.run")
	if run.Status().Code != codes.Error {
		t.Fatalf("agentcore.run status = %v after a contained panic, want Error", run.Status())
	}
	var class string
	for _, a := range run.Attributes() {
		if a.Key == "error.type" {
			class = a.Value.AsString()
		}
	}
	if class == "" {
		t.Fatal("agentcore.run carries no error.type after a contained panic")
	}
}
