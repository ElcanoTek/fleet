package agentcore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"

	"github.com/ElcanoTek/fleet/internal/metrics"
)

// Inner-step context budgeting runs before EVERY Fantasy provider request,
// including successive tool steps within one outer agentcore round. The outer
// pressure check cannot protect that accumulation because Fantasy owns those
// inner steps.
const (
	innerProviderReserveMinTokens = 4096
	innerProviderReserveDivisor   = 20 // 5% framing/tokenizer uncertainty
	innerResultPreviewBytes       = 2048
	innerResultEvictedBytes       = 640
	innerInputEvictedBytes        = 512
	messageFramingTokens          = 4
	partFramingTokens             = 2
)

// ErrInnerContextBudgetExceeded prevents a provider request when system prompt,
// schemas, completion headroom, and irreducible messages cannot fit the active
// model's window even after every tool payload has been reduced.
var ErrInnerContextBudgetExceeded = errors.New("inner model context budget exceeded")

type modelContextPrefixBudget struct {
	systemTokens int
	toolTokens   int
	systemPrompt string
}

func buildModelContextPrefixBudget(systemPrompt string, registeredTools []fantasy.AgentTool) modelContextPrefixBudget {
	systemTokens := 0
	if systemPrompt != "" {
		systemTokens = estimateModelMessagesTokens([]fantasy.Message{fantasy.NewSystemMessage(systemPrompt)})
	}
	return modelContextPrefixBudget{
		systemTokens: systemTokens,
		toolTokens:   estimateToolSchemaTokens(registeredTools),
		systemPrompt: systemPrompt,
	}
}

func (e *engine) setModelContextPrefix(systemPrompt string, registeredTools []fantasy.AgentTool) {
	if e == nil {
		return
	}
	e.modelContextPrefix = buildModelContextPrefixBudget(systemPrompt, registeredTools)
}

// ModelContextBudgetStep exposes the shared aggregate guard for auxiliary
// Fantasy calls that replay agentcore.Run's governed history (interactive
// finalize/recovery). Normal interactive and scheduled execution is wired in
// roundState.stream and does not need to call this directly.
func ModelContextBudgetStep(systemPrompt string, registeredTools []fantasy.AgentTool, maxCompletionTokens int) fantasy.PrepareStepFunction {
	prefix := buildModelContextPrefixBudget(systemPrompt, registeredTools)
	return modelContextBudgetStep(prefix, maxCompletionTokens, nil)
}

type innerContextAccounting struct {
	window           int
	systemTokens     int
	toolTokens       int
	completionTokens int
	providerTokens   int
	messageTarget    int
}

func contextAccounting(prefix modelContextPrefixBudget, maxCompletionTokens, window int) innerContextAccounting {
	completion := maxCompletionTokens
	if completion <= 0 {
		completion = DefaultMaxCompletionTokens
	}
	// Reserve the exact allowance sent to the provider. Clamping only this
	// accounting copy would claim headroom that the actual MaxOutputTokens may
	// consume; an impossible setting is refused below instead.
	provider := max(innerProviderReserveMinTokens, window/innerProviderReserveDivisor)
	// Checked subtraction avoids an extreme live setting overflowing an int and
	// turning an impossible reserve into a positive message target.
	messageTarget := window
	for _, reserve := range []int{prefix.systemTokens, prefix.toolTokens, completion, provider} {
		if reserve < 0 || reserve >= messageTarget {
			messageTarget = -1
			break
		}
		messageTarget -= reserve
	}
	return innerContextAccounting{
		window:           window,
		systemTokens:     prefix.systemTokens,
		toolTokens:       prefix.toolTokens,
		completionTokens: completion,
		providerTokens:   provider,
		messageTarget:    messageTarget,
	}
}

func (a innerContextAccounting) reservedTokens() int {
	if a.messageTarget < 0 {
		return a.window + 1
	}
	return a.window - a.messageTarget
}

type innerContextReduction struct {
	resultPreviews int
	resultEvicts   int
	inputEvicts    int
}

func modelContextBudgetStep(prefix modelContextPrefixBudget, maxCompletionTokens int, sink *streamSink) fantasy.PrepareStepFunction {
	return func(ctx context.Context, opts fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
		modelSlug := ""
		if opts.Model != nil {
			modelSlug = opts.Model.Model()
		}
		window := contextWindowForActiveModel(opts.Model)
		accounting := contextAccounting(prefix, maxCompletionTokens, window)
		if accounting.messageTarget <= 0 {
			return ctx, fantasy.PrepareStepResult{}, fmt.Errorf(
				"%w: fixed reserves exceed window (model=%s window=%d system=%d schemas=%d completion=%d provider=%d)",
				ErrInnerContextBudgetExceeded, modelSlug, window, accounting.systemTokens, accounting.toolTokens,
				accounting.completionTokens, accounting.providerTokens)
		}

		beforeMessages := estimateBudgetMessagesTokens(opts.Messages, prefix)
		beforeTotal := beforeMessages + accounting.reservedTokens()
		recordInnerContextMetrics(modelSlug, "before", beforeTotal, window)
		recordInnerContextMetrics(modelSlug, "target", accounting.messageTarget, window)
		recordInnerContextMetrics(modelSlug, "reserved", accounting.reservedTokens(), window)

		// Always normalize legacy replay payloads that predate the final hard
		// boundary, even when this particular model has enough aggregate room.
		messages := cloneFantasyMessages(opts.Messages)
		toolNames := toolNamesByCallID(messages)
		reduction := reduceHistoricalPayloadsToHardCap(messages, toolNames)
		messageTokens := estimateBudgetMessagesTokens(messages, prefix)

		if messageTokens > accounting.messageTarget {
			reduction.resultPreviews += compactOldToolResults(messages, toolNames, prefix, accounting.messageTarget, innerResultPreviewBytes)
			messageTokens = estimateBudgetMessagesTokens(messages, prefix)
		}
		if messageTokens > accounting.messageTarget {
			reduction.inputEvicts += evictOldToolInputs(messages, prefix, accounting.messageTarget)
			messageTokens = estimateBudgetMessagesTokens(messages, prefix)
		}
		if messageTokens > accounting.messageTarget {
			reduction.resultEvicts += compactOldToolResults(messages, toolNames, prefix, accounting.messageTarget, innerResultEvictedBytes)
			messageTokens = estimateBudgetMessagesTokens(messages, prefix)
		}

		afterTotal := messageTokens + accounting.reservedTokens()
		recordInnerContextMetrics(modelSlug, "after", afterTotal, window)
		metrics.RecordToolContextReduction("result_preview", reduction.resultPreviews)
		metrics.RecordToolContextReduction("result_evict", reduction.resultEvicts)
		metrics.RecordToolContextReduction("input_evict", reduction.inputEvicts)

		if reduction.resultPreviews+reduction.resultEvicts+reduction.inputEvicts > 0 && sink != nil {
			sink.emit("fleet.tool_context_reduced", map[string]any{
				"estimated_tokens_before": beforeTotal,
				"estimated_tokens_after":  afterTotal,
				"message_token_target":    accounting.messageTarget,
				"reserved_tokens":         accounting.reservedTokens(),
				"result_previews":         reduction.resultPreviews,
				"result_evictions":        reduction.resultEvicts,
				"input_evictions":         reduction.inputEvicts,
			})
		}
		if messageTokens > accounting.messageTarget {
			return ctx, fantasy.PrepareStepResult{}, fmt.Errorf(
				"%w: irreducible messages exceed reserved target (model=%s messages=%d target=%d window=%d reserved=%d)",
				ErrInnerContextBudgetExceeded, modelSlug, messageTokens, accounting.messageTarget, window, accounting.reservedTokens())
		}
		if reduction.resultPreviews+reduction.resultEvicts+reduction.inputEvicts == 0 {
			return ctx, fantasy.PrepareStepResult{}, nil
		}
		return ctx, fantasy.PrepareStepResult{Messages: messages}, nil
	}
}

func recordInnerContextMetrics(model, phase string, tokens, window int) {
	ratio := 0.0
	if window > 0 {
		ratio = float64(tokens) / float64(window)
	}
	metrics.RecordToolContextPressure(model, phase, tokens, ratio)
}

func estimateToolSchemaTokens(registeredTools []fantasy.AgentTool) int {
	total := 0
	for _, tool := range registeredTools {
		if tool == nil {
			continue
		}
		encoded, err := json.Marshal(tool.Info())
		if err != nil {
			// A schema that cannot marshal will fail registration/provider use
			// elsewhere. Reserve the conservative per-tool fallback meanwhile.
			total += tokensPerMCPTool
			continue
		}
		total += estimatedTokensForBytes(len(encoded)) + 8
	}
	return total
}

func estimatedTokensForBytes(n int) int {
	if n <= 0 {
		return 0
	}
	return (n + charsPerToken - 1) / charsPerToken
}

// estimateModelMessagesTokens includes every model-visible payload class the
// old overflow estimator missed: text, tool results (including error/media),
// and assistant tool-call inputs. The configured system prompt is reserved
// separately; any system-role message in history is additional provider input
// and must still be counted here.
func estimateModelMessagesTokens(messages []fantasy.Message) int {
	total := 0
	for _, message := range messages {
		total += messageFramingTokens
		for _, part := range message.Content {
			total += partFramingTokens
			if p, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
				total += estimatedTokensForBytes(len(p.Text))
				continue
			}
			if p, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok {
				total += estimatedTokensForBytes(len(p.ToolCallID) + len(p.ToolName) + len(p.Input))
				continue
			}
			if p, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
				total += estimateToolResultTokens(p.Output)
				continue
			}
			if p, ok := fantasy.AsMessagePart[fantasy.ReasoningPart](part); ok {
				total += estimatedTokensForBytes(len(p.Text))
				if metadata := anthropic.GetReasoningMetadata(p.ProviderOptions); metadata != nil {
					// Anthropic serializes these fields back into provider-visible
					// thinking/redacted-thinking blocks. RedactedData in particular can
					// be large even when Text is empty, so omitting it bypasses the guard.
					total += estimatedTokensForBytes(len(metadata.Signature) + len(metadata.RedactedData))
				}
				if metadata := openai.GetReasoningMetadata(p.ProviderOptions); metadata != nil {
					bytes := len(metadata.ItemID)
					if metadata.EncryptedContent != nil {
						bytes += len(*metadata.EncryptedContent)
					}
					for _, summary := range metadata.Summary {
						bytes += len(summary)
					}
					total += estimatedTokensForBytes(bytes)
				}
				if metadata := google.GetReasoningMetadata(p.ProviderOptions); metadata != nil {
					total += estimatedTokensForBytes(len(metadata.Signature) + len(metadata.ToolID))
				}
				continue
			}
			if p, ok := fantasy.AsMessagePart[fantasy.FilePart](part); ok {
				// Providers encode binary files as base64 on the wire. Account for
				// that expansion rather than treating raw bytes as prompt bytes.
				encodedDataBytes := ((len(p.Data) + 2) / 3) * 4
				total += estimatedTokensForBytes(len(p.Filename) + encodedDataBytes + len(p.MediaType))
			}
		}
	}
	return total
}

// Fantasy prepends WithSystemPrompt to PrepareStep.Options.Messages. Reserve
// that exact message in prefix.systemTokens and remove the matching leading
// copy from history accounting so it is counted once—not twice. A user-supplied
// later system-role message does not match and remains fully counted.
func estimateBudgetMessagesTokens(messages []fantasy.Message, prefix modelContextPrefixBudget) int {
	total := estimateModelMessagesTokens(messages)
	if len(messages) == 0 || prefix.systemPrompt == "" || messages[0].Role != fantasy.MessageRoleSystem || len(messages[0].Content) != 1 {
		return total
	}
	text, ok := fantasy.AsMessagePart[fantasy.TextPart](messages[0].Content[0])
	if !ok || text.Text != prefix.systemPrompt {
		return total
	}
	return max(0, total-estimateModelMessagesTokens(messages[:1]))
}

func estimateToolResultTokens(output fantasy.ToolResultOutputContent) int {
	if out, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](output); ok {
		return estimatedTokensForBytes(len(out.Text))
	}
	if out, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](output); ok {
		if out.Error != nil {
			return estimatedTokensForBytes(len(out.Error.Error()))
		}
		return 0
	}
	if out, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](output); ok {
		return estimatedTokensForBytes(len(out.Data) + len(out.MediaType) + len(out.Text))
	}
	return 0
}

func cloneFantasyMessages(in []fantasy.Message) []fantasy.Message {
	out := make([]fantasy.Message, len(in))
	for i, message := range in {
		out[i] = message
		out[i].Content = append([]fantasy.MessagePart(nil), message.Content...)
	}
	return out
}

func toolNamesByCallID(messages []fantasy.Message) map[string]string {
	names := make(map[string]string)
	for _, message := range messages {
		for _, part := range message.Content {
			if call, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok {
				names[call.ToolCallID] = call.ToolName
			}
		}
	}
	return names
}

func reduceHistoricalPayloadsToHardCap(messages []fantasy.Message, toolNames map[string]string) innerContextReduction {
	var reduced innerContextReduction
	for mi := range messages {
		for pi, part := range messages[mi].Content {
			if p, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
				text, ok := toolResultOutputText(p.Output)
				_, isMedia := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](p.Output)
				if !ok || (!isMedia && len(text) <= HardMaxToolOutputBytes) {
					continue
				}
				if isMedia {
					// toolResultOutputText already discarded the media data and
					// rendered an honest binary_suppressed envelope. Do not wrap it
					// again and accidentally lose that metadata.
					p.Output = fantasy.ToolResultOutputContentText{Text: text}
				} else {
					p.Output = replaceToolResultOutput(p.Output, aggregateResultEnvelope(toolNames[p.ToolCallID], text, innerResultPreviewBytes))
				}
				messages[mi].Content[pi] = p
				reduced.resultPreviews++
				continue
			}
			if p, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok {
				if len(p.Input) <= HardMaxToolOutputBytes {
					continue
				}
				p.Input = aggregateInputEnvelope(p.ToolName, len(p.Input), innerInputEvictedBytes)
				messages[mi].Content[pi] = p
				reduced.inputEvicts++
			}
		}
	}
	return reduced
}

func compactOldToolResults(messages []fantasy.Message, toolNames map[string]string, prefix modelContextPrefixBudget, targetTokens, maxBytes int) int {
	count := 0
	for mi := range messages {
		for pi, part := range messages[mi].Content {
			if estimateBudgetMessagesTokens(messages, prefix) <= targetTokens {
				return count
			}
			p, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
			if !ok {
				continue
			}
			text, ok := toolResultOutputText(p.Output)
			if !ok || len(text) <= maxBytes {
				continue
			}
			replacement := aggregateResultEnvelope(toolNames[p.ToolCallID], text, maxBytes)
			if len(replacement) >= len(text) {
				continue
			}
			p.Output = replaceToolResultOutput(p.Output, replacement)
			messages[mi].Content[pi] = p
			count++
		}
	}
	return count
}

func evictOldToolInputs(messages []fantasy.Message, prefix modelContextPrefixBudget, targetTokens int) int {
	count := 0
	for mi := range messages {
		for pi, part := range messages[mi].Content {
			if estimateBudgetMessagesTokens(messages, prefix) <= targetTokens {
				return count
			}
			p, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part)
			if !ok || len(p.Input) <= innerInputEvictedBytes {
				continue
			}
			p.Input = aggregateInputEnvelope(p.ToolName, len(p.Input), innerInputEvictedBytes)
			messages[mi].Content[pi] = p
			count++
		}
	}
	return count
}

func toolResultOutputText(output fantasy.ToolResultOutputContent) (text string, ok bool) {
	if out, matched := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](output); matched {
		return out.Text, true
	}
	if out, matched := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](output); matched {
		if out.Error == nil {
			return "", true
		}
		return out.Error.Error(), true
	}
	if out, matched := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](output); matched {
		envelope := toolOutputEnvelope{
			Tool:             "tool",
			OriginalBytes:    len(out.Data),
			Truncated:        true,
			Format:           "media",
			BinarySuppressed: true,
			MediaType:        out.MediaType,
			RecoveryAction:   "Re-run the tool so it saves media to a workspace-relative path.",
		}
		return renderJSONEnvelope(envelope, "", HardMaxToolOutputBytes), true
	}
	return "", false
}

func replaceToolResultOutput(original fantasy.ToolResultOutputContent, replacement string) fantasy.ToolResultOutputContent {
	if _, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](original); ok {
		return fantasy.ToolResultOutputContentError{Error: errors.New(replacement)}
	}
	return fantasy.ToolResultOutputContentText{Text: replacement}
}

func aggregateResultEnvelope(toolName, content string, maxBytes int) string {
	format := "text"
	if json.Valid([]byte(strings.TrimSpace(content))) {
		format = "json"
	}
	originalBytes, artifactPath, binarySuppressed := existingEnvelopeMetadata(content)
	if originalBytes <= 0 {
		originalBytes = len(content)
	}
	recovery := fmt.Sprintf("Re-run %s with narrower output.", boundedToolName(toolName))
	if artifactPath != "" {
		recovery = fmt.Sprintf("Use view_file with path %q to inspect the governed full output.", artifactPath)
	}
	env := toolOutputEnvelope{
		Tool:             boundedToolName(toolName),
		OriginalBytes:    originalBytes,
		Truncated:        true,
		Format:           format,
		BinarySuppressed: binarySuppressed || containsEncodedBinary(content),
		ArtifactPath:     artifactPath,
		RecoveryAction:   recovery,
	}
	if format == "json" {
		return renderJSONEnvelope(env, content, maxBytes)
	}
	return renderTextEnvelope(env, content, maxBytes)
}

func existingEnvelopeMetadata(content string) (originalBytes int, artifactPath string, binarySuppressed bool) {
	if len(content) > HardMaxToolOutputBytes {
		return 0, "", false
	}
	var envelope toolOutputEnvelope
	if json.Unmarshal([]byte(content), &envelope) == nil && validFleetOutputEnvelope(envelope) {
		return envelope.OriginalBytes, validatedArtifactPath(envelope.ArtifactPath), envelope.BinarySuppressed
	}
	if !strings.HasPrefix(content, "[tool output truncated]\n") {
		return 0, "", false
	}
	var truncated, hasRecovery, validFormat bool
	for _, line := range strings.Split(content, "\n") {
		// Only the outer Fleet envelope is authoritative. Its preview may itself
		// contain a previously bounded envelope (or attacker-controlled lines) and
		// must never overwrite the recovery metadata parsed above.
		if line == "preview:" {
			break
		}
		if line == "truncated: true" {
			truncated = true
		}
		if strings.HasPrefix(line, "original_bytes: ") {
			originalBytes, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "original_bytes: ")))
		}
		if strings.HasPrefix(line, "artifact_path: ") {
			artifactPath = validatedArtifactPath(strings.TrimSpace(strings.TrimPrefix(line, "artifact_path: ")))
		}
		if strings.HasPrefix(line, "format: ") {
			validFormat = validToolOutputFormat(strings.TrimSpace(strings.TrimPrefix(line, "format: ")))
		}
		if strings.HasPrefix(line, "recovery_action: ") && strings.TrimSpace(strings.TrimPrefix(line, "recovery_action: ")) != "" {
			hasRecovery = true
		}
		if line == "binary_suppressed: true" {
			binarySuppressed = true
		}
	}
	if !truncated || originalBytes <= 0 || !hasRecovery || !validFormat {
		return 0, "", false
	}
	return originalBytes, artifactPath, binarySuppressed
}

func validFleetOutputEnvelope(envelope toolOutputEnvelope) bool {
	return envelope.FleetEnvelope == fleetToolOutputEnvelopeV1 && envelope.Truncated && envelope.OriginalBytes > 0 && envelope.RecoveryAction != "" && validToolOutputFormat(envelope.Format)
}

func validToolOutputFormat(format string) bool {
	return format == "text" || format == "json" || format == "media"
}

func validatedArtifactPath(path string) string {
	const prefix = ".fleet/tool-output/slot-"
	const middle = "/artifact-"
	const digestHexBytes = 64
	const suffix = ".txt"
	if len(path) != len(prefix)+2+len(middle)+digestHexBytes+len(suffix) ||
		!strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return ""
	}
	digits := path[len(prefix) : len(prefix)+2]
	slot, err := strconv.Atoi(digits)
	if err != nil || slot < 0 || slot >= 16 {
		return ""
	}
	if path[len(prefix)+2:len(prefix)+2+len(middle)] != middle {
		return ""
	}
	digest := path[len(prefix)+2+len(middle) : len(path)-len(suffix)]
	for i := range digest {
		if (digest[i] < '0' || digest[i] > '9') && (digest[i] < 'a' || digest[i] > 'f') {
			return ""
		}
	}
	return path
}

func aggregateInputEnvelope(toolName string, originalBytes, maxBytes int) string {
	envelope := map[string]any{
		"_fleet_context_evicted": true,
		"tool":                   boundedToolName(toolName),
		"original_bytes":         originalBytes,
		"recovery_action":        "Repeat the call with smaller arguments or pass a workspace-relative file path.",
	}
	encoded, _ := json.Marshal(envelope)
	if len(encoded) <= maxBytes {
		return string(encoded)
	}
	return fmt.Sprintf(`{"_fleet_context_evicted":true,"original_bytes":%d}`, originalBytes)
}

func chainPrepareStepFunctions(steps ...fantasy.PrepareStepFunction) fantasy.PrepareStepFunction {
	return func(ctx context.Context, opts fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
		var final fantasy.PrepareStepResult
		for _, step := range steps {
			if step == nil {
				continue
			}
			stepCtx, result, err := step(ctx, opts)
			if err != nil {
				return ctx, fantasy.PrepareStepResult{}, err
			}
			ctx = stepCtx
			if result.Messages != nil {
				opts.Messages = result.Messages
				final.Messages = result.Messages
			}
			if result.Model != nil {
				opts.Model = result.Model
				final.Model = result.Model
			}
			if result.System != nil {
				final.System = result.System
			}
			if result.ToolChoice != nil {
				final.ToolChoice = result.ToolChoice
			}
			if result.ActiveTools != nil {
				final.ActiveTools = result.ActiveTools
			}
			if result.Tools != nil {
				final.Tools = result.Tools
			}
			final.DisableAllTools = final.DisableAllTools || result.DisableAllTools
		}
		return ctx, final, nil
	}
}
