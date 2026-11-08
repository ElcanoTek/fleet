package agentcore

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"go.opentelemetry.io/otel/codes"

	"github.com/ElcanoTek/fleet/internal/telemetry/spantest"
)

// panickingModel panics in every provider call.
type panickingModel struct{ fantasy.LanguageModel }

func (panickingModel) Provider() string { return "test" }
func (panickingModel) Model() string    { return "panics" }
func (panickingModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	panic("provider exploded")
}
func (panickingModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	panic("provider exploded")
}
func (panickingModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	panic("provider exploded")
}
func (panickingModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	panic("provider exploded")
}

// panickingStreamModel returns a stream whose iteration panics.
type panickingStreamModel struct{ panickingModel }

func (panickingStreamModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	return func(func(fantasy.StreamPart) bool) { panic("stream exploded") }, nil
}

func mustPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("expected the provider panic to propagate")
		}
	}()
	fn()
}

// A provider panic inside a model call (or before a stream is handed back,
// or while it is iterated) ends the model span with a "panic" error and
// re-raises the panic unchanged (#2052).
func TestModelSpansEndOnProviderPanic(t *testing.T) {
	sr := spantest.Install(t)
	m := withModelTracing(panickingModel{})
	ctx := context.Background()

	mustPanic(t, func() { _, _ = m.Generate(ctx, fantasy.Call{}) })
	mustPanic(t, func() { _, _ = m.Stream(ctx, fantasy.Call{}) })
	mustPanic(t, func() { _, _ = m.GenerateObject(ctx, fantasy.ObjectCall{}) })
	mustPanic(t, func() { _, _ = m.StreamObject(ctx, fantasy.ObjectCall{}) })
	seq, err := withModelTracing(panickingStreamModel{}).Stream(ctx, fantasy.Call{})
	if err != nil {
		t.Fatal(err)
	}
	mustPanic(t, func() {
		for range seq {
		}
	})

	sr.AssertAllEnded(t)
	for _, s := range sr.Ended() {
		spantest.AssertStatus(t, s, codes.Error, "panic")
	}
	if n := len(sr.Ended()); n != 5 {
		t.Fatalf("%d model spans ended, want 5", n)
	}
}
