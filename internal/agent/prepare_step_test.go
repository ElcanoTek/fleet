package agent

import (
	"context"
	"errors"
	"testing"

	"charm.land/fantasy"
)

func TestChainPrepareStepsOrderAndPassThrough(t *testing.T) {
	noop := func(ctx context.Context, _ fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
		return ctx, fantasy.PrepareStepResult{}, nil
	}
	appendText := func(suffix string) fantasy.PrepareStepFunction {
		return func(ctx context.Context, opts fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
			messages := append([]fantasy.Message(nil), opts.Messages...)
			part := messages[0].Content[0].(fantasy.TextPart)
			messages[0].Content = []fantasy.MessagePart{fantasy.TextPart{Text: part.Text + suffix}}
			return ctx, fantasy.PrepareStepResult{Messages: messages}, nil
		}
	}

	chain := chainPrepareSteps(noop, appendText("a"), noop, appendText("b"))
	_, out, err := chain(context.Background(), fantasy.PrepareStepFunctionOptions{Messages: []fantasy.Message{{
		Role:    fantasy.MessageRoleUser,
		Content: []fantasy.MessagePart{fantasy.TextPart{Text: "start-"}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := out.Messages[0].Content[0].(fantasy.TextPart).Text; got != "start-ab" {
		t.Fatalf("steps ran out of order: %q", got)
	}
}

func TestChainPrepareStepsPropagatesError(t *testing.T) {
	want := errors.New("boom")
	bad := func(ctx context.Context, _ fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
		return ctx, fantasy.PrepareStepResult{}, want
	}
	after := func(ctx context.Context, _ fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
		t.Fatal("step after error ran")
		return ctx, fantasy.PrepareStepResult{}, nil
	}

	_, _, err := chainPrepareSteps(bad, after)(context.Background(), fantasy.PrepareStepFunctionOptions{})
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

func TestChainPrepareStepsNilsAndSingleton(t *testing.T) {
	if chainPrepareSteps() != nil || chainPrepareSteps(nil, nil) != nil {
		t.Fatal("empty chain should be nil")
	}
	single := func(ctx context.Context, opts fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
		return ctx, fantasy.PrepareStepResult{Messages: opts.Messages}, nil
	}
	if chainPrepareSteps(nil, single, nil) == nil {
		t.Fatal("singleton chain should be preserved")
	}
}
