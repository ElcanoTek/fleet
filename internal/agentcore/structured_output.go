package agentcore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

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

func validateDeclaredOutputSchema(schemaRaw json.RawMessage) error {
	if len(schemaRaw) == 0 {
		return nil
	}
	if err := structuredoutput.ValidateSchema(schemaRaw); err != nil {
		return fmt.Errorf("%w: declared schema is invalid: %w", ErrStructuredOutputGeneration, err)
	}
	return nil
}

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
	if err := validateDeclaredOutputSchema(schemaRaw); err != nil {
		return nil, err
	}
	var schemaObject map[string]any
	decoder := json.NewDecoder(bytes.NewReader(schemaRaw))
	// Preserve schema numbers exactly on the provider wire. The default
	// json.Unmarshal path converts every number to float64, which silently rounds
	// integers above 2^53 and can make the provider enforce a different schema
	// from Fleet's local validator.
	decoder.UseNumber()
	if err := decoder.Decode(&schemaObject); err != nil {
		return nil, fmt.Errorf("%w: decode schema: %w", ErrStructuredOutputGeneration, err)
	}
	providerSchema, enveloped := terminalProviderSchema(schemaObject)

	prompt := make(fantasy.Prompt, 0, len(messages)+2+StructuredOutputCorrectionAttempts)
	if strings.TrimSpace(systemPrompt) != "" {
		prompt = append(prompt, fantasy.NewSystemMessage(systemPrompt))
	}
	prompt = append(prompt, messages...)
	terminalInstruction := "Produce the required terminal structured output now from the completed work above. " +
		"Do not repeat any tool work or side effects."
	if enveloped {
		terminalInstruction += " The provider schema wraps the declared result in a top-level value field; put the complete result there."
	}
	prompt = append(prompt, fantasy.NewUserMessage(terminalInstruction))

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
			call.ProviderOptions = nativeStructuredOutputOptions(call.ProviderOptions, providerSchema)
		} else {
			toolChoice := fantasy.SpecificToolChoice(structuredOutputToolName)
			call.Tools = []fantasy.Tool{fantasy.FunctionTool{
				Name:        structuredOutputToolName,
				Description: "Submit the terminal machine-readable result. This is the only allowed terminal action.",
				InputSchema: providerSchema,
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

		candidate, kind, detail := terminalCandidate(resp, native, enveloped)
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

// terminalProviderSchema keeps the declared schema semantically equivalent on
// the common object-root path. Function-tool and strict-response providers
// generally require their top-level schema to be an object, though Fleet's
// existing public validator also accepts array/scalar result schemas. Wrap only
// those non-definitely-object schemas in a deterministic value envelope, then
// unwrap before local validation and persistence.
func terminalProviderSchema(schema map[string]any) (map[string]any, bool) {
	if rootType, ok := schema["type"].(string); ok && rootType == "object" {
		return schema, false
	}
	// Moving a schema beneath properties.value changes the document root seen by
	// fragment-only JSON Pointer refs. Rebase refs that still belong to the root
	// resource so # and #/definitions/... retain exactly the semantics Fleet
	// validated locally. Nested schemas with their own non-fragment $id establish
	// a separate resource and keep their own fragment namespace.
	valueSchema := cloneTerminalSchema(schema)
	rebaseTerminalSchemaRefs(valueSchema, "#/properties/value", true)
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"value": valueSchema,
		},
		"required":             []string{"value"},
		"additionalProperties": false,
	}, true
}

func cloneTerminalSchema(schema map[string]any) map[string]any {
	cloned, _ := cloneTerminalSchemaValue(schema).(map[string]any)
	return cloned
}

func cloneTerminalSchemaValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, child := range value {
			out[key] = cloneTerminalSchemaValue(child)
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, child := range value {
			out[i] = cloneTerminalSchemaValue(child)
		}
		return out
	default:
		return value
	}
}

// rebaseTerminalSchemaRefs walks schema-valued keywords only. A generic JSON
// walk would corrupt literal data such as {"const":{"$ref":"#"}} even though
// that nested $ref-shaped field is an instance value, not a schema reference.
func rebaseTerminalSchemaRefs(schema map[string]any, prefix string, rebase bool) {
	if id, ok := schema["$id"].(string); ok && id != "" && !strings.HasPrefix(id, "#") {
		rebase = false
	}
	if rebase {
		for _, keyword := range []string{"$ref", "$recursiveRef", "$dynamicRef"} {
			ref, ok := schema[keyword].(string)
			if !ok {
				continue
			}
			switch {
			case ref == "#":
				schema[keyword] = prefix
			case strings.HasPrefix(ref, "#/"):
				schema[keyword] = prefix + ref[1:]
			}
		}
	}

	for _, keyword := range []string{"$defs", "definitions", "properties", "patternProperties", "dependentSchemas"} {
		children, ok := schema[keyword].(map[string]any)
		if !ok {
			continue
		}
		for _, child := range children {
			rebaseTerminalSchemaValue(child, prefix, rebase)
		}
	}
	for _, keyword := range []string{
		"additionalProperties", "unevaluatedProperties", "propertyNames", "contains",
		"items", "additionalItems", "unevaluatedItems", "not", "if", "then", "else", "contentSchema",
	} {
		rebaseTerminalSchemaValue(schema[keyword], prefix, rebase)
	}
	for _, keyword := range []string{"prefixItems", "allOf", "anyOf", "oneOf"} {
		rebaseTerminalSchemaValue(schema[keyword], prefix, rebase)
	}
	// Draft-07 dependencies values are either a subschema or an array of property
	// names. The helper deliberately ignores the latter.
	if dependencies, ok := schema["dependencies"].(map[string]any); ok {
		for _, child := range dependencies {
			rebaseTerminalSchemaValue(child, prefix, rebase)
		}
	}
}

func rebaseTerminalSchemaValue(value any, prefix string, rebase bool) {
	switch value := value.(type) {
	case map[string]any:
		rebaseTerminalSchemaRefs(value, prefix, rebase)
	case []any:
		for _, child := range value {
			if childSchema, ok := child.(map[string]any); ok {
				rebaseTerminalSchemaRefs(childSchema, prefix, rebase)
			}
		}
	}
}

func terminalCandidate(resp *fantasy.Response, native, enveloped bool) (string, terminalOutputKind, string) {
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
			// OpenAI-compatible chat responses carry strict-schema refusals in a
			// separate message.refusal field. Fantasy's current OpenRouter adapter
			// does not expose that field, leaving an otherwise successful stop with
			// no content. In strict native mode that shape cannot be valid JSON, so
			// preserve the public refusal classification instead of wasting the
			// correction budget on an adapter-level empty response.
			if resp.FinishReason == fantasy.FinishReasonStop {
				return "", terminalOutputRefusal, "native strict provider returned an empty stop/refusal"
			}
			return "", terminalOutputMissing, "native structured response contained no JSON text"
		}
		if !enveloped {
			return text, terminalOutputValid, ""
		}
		return unwrapTerminalEnvelope(text)
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
	if !enveloped {
		return calls[0].Input, terminalOutputValid, ""
	}
	return unwrapTerminalEnvelope(calls[0].Input)
}

func unwrapTerminalEnvelope(candidate string) (string, terminalOutputKind, string) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(candidate), &envelope); err != nil {
		return "", terminalOutputInvalid, "terminal value envelope was not a JSON object"
	}
	value, ok := envelope["value"]
	if !ok || len(bytes.TrimSpace(value)) == 0 {
		return "", terminalOutputMissing, "terminal value envelope omitted the required value field"
	}
	if len(envelope) != 1 {
		return "", terminalOutputInvalid, "terminal value envelope contained fields other than value"
	}
	return string(value), terminalOutputValid, ""
}

func clampCorrectionDetail(s string) string {
	s = strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(s))
	if len(s) <= maxStructuredCorrectionDetailBytes {
		return s
	}
	const truncationMarker = "..."
	cut := maxStructuredCorrectionDetailBytes - len(truncationMarker)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + truncationMarker
}
