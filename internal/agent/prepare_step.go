package agent

import (
	"context"

	"charm.land/fantasy"
)

// chainPrepareSteps composes multiple PrepareStep functions into one. Each
// step sees the messages produced by the previous step. A nil Messages result
// is pass-through. Fleet's model-aware context budget now owns all history
// reduction; this helper only composes it with provider prompt caching.
func chainPrepareSteps(steps ...fantasy.PrepareStepFunction) fantasy.PrepareStepFunction {
	nonNil := make([]fantasy.PrepareStepFunction, 0, len(steps))
	for _, step := range steps {
		if step != nil {
			nonNil = append(nonNil, step)
		}
	}
	if len(nonNil) == 0 {
		return nil
	}
	if len(nonNil) == 1 {
		return nonNil[0]
	}

	return func(ctx context.Context, opts fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
		messages := opts.Messages
		var final fantasy.PrepareStepResult
		for _, step := range nonNil {
			stepCtx, out, err := step(ctx, fantasy.PrepareStepFunctionOptions{
				Model:      opts.Model,
				Steps:      opts.Steps,
				StepNumber: opts.StepNumber,
				Messages:   messages,
			})
			if err != nil {
				return ctx, fantasy.PrepareStepResult{}, err
			}
			ctx = stepCtx
			if out.Messages != nil {
				messages = out.Messages
			}
			final = mergePrepareStepResult(final, out)
		}
		final.Messages = messages
		return ctx, final, nil
	}
}

// mergePrepareStepResult combines non-message fields from b into a. Later
// steps win; Messages is handled by chainPrepareSteps.
func mergePrepareStepResult(a, b fantasy.PrepareStepResult) fantasy.PrepareStepResult {
	if b.Model != nil {
		a.Model = b.Model
	}
	if b.Tools != nil {
		a.Tools = b.Tools
	}
	if b.ToolChoice != nil {
		a.ToolChoice = b.ToolChoice
	}
	if b.System != nil {
		a.System = b.System
	}
	if b.ActiveTools != nil {
		a.ActiveTools = b.ActiveTools
	}
	if b.DisableAllTools {
		a.DisableAllTools = true
	}
	return a
}
