package agentcore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openrouter"

	"github.com/ElcanoTek/fleet/internal/structuredoutput"
)

const (
	// StructuredOutputCorrectionAttempts is the dedicated correction budget
	// after the first terminal generation. It is deliberately independent of
	// MaxIterations: correction cannot reopen the ordinary agent/tool loop.
	StructuredOutputCorrectionAttempts = 2
	structuredOutputToolName           = "structured_output"
	maxStructuredCorrectionDetailBytes = 1024
)

var (
	// ErrStructuredOutputFormat is the retry-policy umbrella for failures where
	// the model did not satisfy a declared output contract.
	ErrStructuredOutputFormat = errors.New("structured output format failure")
	// The narrower sentinels preserve an actionable terminal diagnostic while
	// still matching ErrStructuredOutputFormat through errors.Is.
	ErrStructuredOutputInvalid    = fmt.Errorf("%w: invalid output", ErrStructuredOutputFormat)
	ErrStructuredOutputMissing    = fmt.Errorf("%w: missing output", ErrStructuredOutputFormat)
	ErrStructuredOutputRefusal    = fmt.Errorf("%w: model refusal", ErrStructuredOutputFormat)
	ErrStructuredOutputGeneration = fmt.Errorf("%w: generation failed", ErrStructuredOutputFormat)
	// ErrStructuredOutputPersistence is emitted by the runner when validated
	// JSON could not be committed under its lease. It is deliberately separate
	// from model formatting so retry/DLQ policy can distinguish infrastructure.
	ErrStructuredOutputPersistence = errors.New("structured output persistence failure")
)

type terminalOutputKind int

const (
	terminalOutputValid terminalOutputKind = iota
	terminalOutputInvalid
	terminalOutputMissing
	terminalOutputRefusal
)

// generateTerminalStructuredOutput is the only post-agent model phase. It runs
// inside agentcore.Run after all ordinary tools and finish gates have completed.
// Each request receives either zero tools (strict native mode) or exactly one
// forced schema tool, so a correction can never repeat an earlier side effect.
func (e *engine) generateTerminalStructuredOutput(
	ctx context.Context,
	model fantasy.LanguageModel,
	systemPrompt string,
	messages []fantasy.Message,
	schemaRaw json.RawMessage,
	maxTokens int64,
	orch *orchestrationState,
) (json.RawMessage, error) {
	if model == nil {
		return nil, fmt.Errorf("%w: no active model", ErrStructuredOutputGeneration)
	}
	if err := structuredoutput.ValidateSchema(schemaRaw); err != nil {
		return nil, fmt.Errorf("%w: declared schema is no longer valid: %w", ErrStructuredOutputGeneration, err)
	}
	var schemaObject map[string]any
	if err := json.Unmarshal(schemaRaw, &schemaObject); err != nil {
		return nil, fmt.Errorf("%w: decode schema: %w", ErrStructuredOutputGeneration, err)
	}

	prompt := make(fantasy.Prompt, 0, len(messages)+2+StructuredOutputCorrectionAttempts)
	if strings.TrimSpace(systemPrompt) != "" {
		prompt = append(prompt, fantasy.NewSystemMessage(systemPrompt))
	}
	prompt = append(prompt, messages...)
	prompt = append(prompt, fantasy.NewUserMessage(
		"Produce the required terminal structured output now from the completed work above. "+
			"Do not repeat any tool work or side effects.",
	))

	lastKind := terminalOutputMissing
	lastDetail := "no structured output was returned"
	for attempt := 0; attempt <= StructuredOutputCorrectionAttempts; attempt++ {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: context cancelled: %w", ErrStructuredOutputGeneration, ctx.Err())
		}
		if orch != nil {
			if blocked, detail := orch.checkCeilings(); blocked {
				return nil, fmt.Errorf("%w before terminal structured output: %s", ErrCostCeilingExceeded, detail)
			}
		}

		call := fantasy.Call{
			Prompt:          prompt,
			MaxOutputTokens: &maxTokens,
			ProviderOptions: e.providerOptions(model.Model()),
		}
		native := supportsNativeStrictStructuredOutput(model)
		if native {
			call.ProviderOptions = nativeStructuredOutputOptions(call.ProviderOptions, schemaObject)
		} else {
			toolChoice := fantasy.SpecificToolChoice(structuredOutputToolName)
			call.Tools = []fantasy.Tool{fantasy.FunctionTool{
				Name:        structuredOutputToolName,
				Description: "Submit the terminal machine-readable result. This is the only allowed terminal action.",
				InputSchema: schemaObject,
			}}
			call.ToolChoice = &toolChoice
		}

		resp, err := model.Generate(ctx, call)
		if err != nil {
			return nil, fmt.Errorf("%w on attempt %d: %w", ErrStructuredOutputGeneration, attempt+1, err)
		}
		if resp == nil {
			return nil, fmt.Errorf("%w on attempt %d: provider returned no response", ErrStructuredOutputGeneration, attempt+1)
		}
		if orch != nil {
			orch.updateUsage(model.Model(), resp.Usage, resp.ProviderMetadata)
			if e != nil && e.usageReporter != nil {
				e.usageReporter(usageSnapshot(orch))
			}
		}
		if resp.FinishReason == fantasy.FinishReasonError {
			return nil, fmt.Errorf("%w on attempt %d: provider returned finish_reason=%s", ErrStructuredOutputGeneration, attempt+1, resp.FinishReason)
		}

		candidate, kind, detail := terminalCandidate(resp, native)
		if kind == terminalOutputRefusal {
			return nil, fmt.Errorf("%w (finish_reason=%s)", ErrStructuredOutputRefusal, resp.FinishReason)
		}
		if kind == terminalOutputValid {
			validated, validationErr := structuredoutput.ValidateOutput(candidate, schemaRaw)
			if validationErr == nil {
				return validated, nil
			}
			kind = terminalOutputInvalid
			detail = validationErr.Error()
		}

		lastKind, lastDetail = kind, clampCorrectionDetail(detail)
		if attempt < StructuredOutputCorrectionAttempts {
			prompt = append(prompt, fantasy.NewUserMessage(fmt.Sprintf(
				"The terminal output was rejected (%s). Correct only the structured output; no ordinary tools are available. Attempt %d of %d.",
				lastDetail, attempt+2, StructuredOutputCorrectionAttempts+1,
			)))
		}
	}

	if lastKind == terminalOutputMissing {
		return nil, fmt.Errorf("%w after %d attempts: %s", ErrStructuredOutputMissing, StructuredOutputCorrectionAttempts+1, lastDetail)
	}
	return nil, fmt.Errorf("%w after %d attempts: %s", ErrStructuredOutputInvalid, StructuredOutputCorrectionAttempts+1, lastDetail)
}

// supportsNativeStrictStructuredOutput is intentionally conservative. Fleet's
// OpenRouter adapter can pass the full raw draft-07 schema through response_format
// without Fantasy's lossy Schema conversion. Other configured providers use the
// exact raw schema as a forced function tool until their adapters expose an
// equally lossless native request seam.
func supportsNativeStrictStructuredOutput(model fantasy.LanguageModel) bool {
	return model != nil && model.Provider() == openrouter.Name
}

func nativeStructuredOutputOptions(base fantasy.ProviderOptions, schema map[string]any) fantasy.ProviderOptions {
	out := make(fantasy.ProviderOptions, len(base)+1)
	for k, v := range base {
		out[k] = v
	}
	var opts openrouter.ProviderOptions
	if existing, ok := base[openrouter.Name].(*openrouter.ProviderOptions); ok && existing != nil {
		opts = *existing
	}
	opts.ExtraBody = cloneAnyMap(opts.ExtraBody)
	opts.ExtraBody["response_format"] = map[string]any{
		"type": "json_schema",
		"json_schema": map[string]any{
			"name":   structuredOutputToolName,
			"strict": true,
			"schema": schema,
		},
	}
	// OpenRouter must not silently route a strict request to an upstream that
	// ignores response_format. Preserve existing routing fields while requiring
	// support for every requested parameter on this terminal call.
	if opts.Provider == nil {
		opts.Provider = &openrouter.Provider{}
	} else {
		providerCopy := *opts.Provider
		opts.Provider = &providerCopy
	}
	require := true
	opts.Provider.RequireParameters = &require
	out[openrouter.Name] = &opts
	return out
}

func cloneAnyMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	return out
}

func terminalCandidate(resp *fantasy.Response, native bool) (string, terminalOutputKind, string) {
	if resp == nil {
		return "", terminalOutputMissing, "provider returned no response"
	}
	if resp.FinishReason == fantasy.FinishReasonContentFilter {
		return "", terminalOutputRefusal, "provider refused the structured response"
	}
	if native {
		if len(resp.Content.ToolCalls()) != 0 {
			return "", terminalOutputInvalid, "native structured response unexpectedly contained a tool call"
		}
		text := strings.TrimSpace(resp.Content.Text())
		if text == "" {
			return "", terminalOutputMissing, "native structured response contained no JSON text"
		}
		return text, terminalOutputValid, ""
	}

	calls := resp.Content.ToolCalls()
	if len(calls) == 0 {
		return "", terminalOutputMissing, "model did not call the required structured_output tool"
	}
	if len(calls) != 1 {
		return "", terminalOutputInvalid, fmt.Sprintf("model called %d terminal tools; exactly one is required", len(calls))
	}
	if calls[0].ToolName != structuredOutputToolName {
		return "", terminalOutputInvalid, fmt.Sprintf("model called terminal tool %q", calls[0].ToolName)
	}
	if strings.TrimSpace(calls[0].Input) == "" {
		return "", terminalOutputMissing, "structured_output tool arguments were empty"
	}
	return calls[0].Input, terminalOutputValid, ""
}

func clampCorrectionDetail(s string) string {
	s = strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(s))
	if len(s) <= maxStructuredCorrectionDetailBytes {
		return s
	}
	const truncationMarker = "..."
	return s[:maxStructuredCorrectionDetailBytes-len(truncationMarker)] + truncationMarker
}
