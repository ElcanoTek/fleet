// Package agent — tool-result overflow protection.
//
// MCP tool results can return multi-MB payloads (e.g. a full segment list
// from an ad-tech DSP) that blow past provider context limits. OpenRouter
// routes to upstreams with a hard 8 MB input cap (e.g. moonshotai/kimi-k2.6
// via Venice) — one fat tool result is enough to fail the whole turn with:
//
//	bad request: The total text input size exceeds 8 MB
//
// This module installs a fantasy PrepareStep that runs before every LLM
// call and truncates oversized tool results in-place, writing the full
// content to a temp file so nothing is lost.
//
// Strategy (mirrors cutlass/internal/agent/agent.go — same library, same
// upstream limits, keep them in sync):
//
//  1. Phase 1 — per-result cap. Any single tool result > 200 KB is
//     replaced with a 4 KB head + 4 KB tail + overflow file reference.
//  2. Phase 2 — cumulative budget. If total message content > 6 MB
//     (leaves 2 MB headroom under the 8 MB provider cap), aggressively
//     re-truncate ALL tool results to 8 KB (2 KB head + 2 KB tail). This
//     catches the case where many sub-threshold results accumulate past
//     the limit in fantasy's inner step loop.
//
// Why PrepareStep and not a between-turn pass: the 8 MB rejection fires
// inside fantasy.Stream()'s inner step loop, which accumulates tool
// results across steps and sends them in a single request. Truncating
// between turns is too late — by then the fatal error has already fired.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/tools"
)

const (
	// maxToolResultInlineBytes is the per-result cap. 128 KB matches the
	// view_file default in server/internal/tools/fs.go — a single view_file
	// read can never exceed the inline cap on its first pass, so the model
	// always gets exactly the bytes it asked for. Aligning with cutlass's
	// sibling constant (both repos use the same library and the same 8 MB
	// upstream limit, so the caps should not drift).
	maxToolResultInlineBytes = 128 * 1024

	// toolResultHeadTailBytes is how many bytes to keep from each end of
	// an oversized result for the inline summary.
	toolResultHeadTailBytes = 4096

	// maxTotalContextBytes is the cumulative byte budget for all message
	// content. 6 MB leaves 2 MB headroom under the 8 MB OpenRouter upstream
	// cap (e.g. Venice / moonshotai/kimi-k2.6).
	maxTotalContextBytes = 6 * 1024 * 1024

	// budgetToolResultSize is the per-result cap applied during Phase 2
	// aggressive re-truncation when the cumulative budget is blown.
	budgetToolResultSize = 8 * 1024

	// budgetHeadTailBytes is how many bytes to keep from each end during
	// Phase 2 re-truncation.
	budgetHeadTailBytes = 2048

	// budgetToolCallInputSize is the per-call-input cap applied during
	// Phase 2 aggressive re-truncation. Smaller than the result-side
	// cap because tool call inputs that survive Phase 1 are usually
	// the "small but many" case — agents that retry the same oversized
	// call a few times. The 2 KB ceiling fits a sentinel JSON object
	// plus a generous error path saved-file pointer.
	budgetToolCallInputSize = 2 * 1024
)

// overflowTruncationStep returns a fantasy PrepareStepFunction that
// truncates oversized tool results before each step is sent to the LLM.
// Safe to chain with other PrepareStep functions via chainPrepareSteps.
//
// The overflow file directory is resolved per-call from the conversation
// ID in ctx (see overflowDirFromContext), so files land inside the
// per-conversation workspace and get cleaned up with it.
func overflowTruncationStep() fantasy.PrepareStepFunction {
	return func(ctx context.Context, opts fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
		dir := overflowDirFromContext(ctx)
		return ctx, fantasy.PrepareStepResult{Messages: truncateToolResults(opts.Messages, dir)}, nil
	}
}

// truncateToolResults applies the two-phase truncation described in the
// package doc. Oversized content is written into `dir` (which callers
// typically resolve per-conversation via overflowDirFromContext). The
// input slice is mutated in place AND returned, so callers can use
// either shape.
//
// Both ToolResultPart.Output (the result text) and ToolCallPart.Input
// (the JSON arguments the model emitted) are subject to truncation —
// the result text gets head/tail byte-slicing, the call input is
// replaced with a JSON sentinel because partial JSON would corrupt the
// downstream provider's parse of the assistant message. See
// truncateToolCallInputs for the call-side details.
func truncateToolResults(messages []fantasy.Message, dir string) []fantasy.Message {
	// Phase 1a: per-result cap on tool results.
	for i := range messages {
		if messages[i].Role != fantasy.MessageRoleTool {
			continue
		}
		for j := range messages[i].Content {
			part := messages[i].Content[j] //nolint:gosec // j ranges over Content; in bounds by construction
			resultPart, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
			if !ok {
				continue
			}
			textOut, ok := resultPart.Output.(fantasy.ToolResultOutputContentText)
			if !ok {
				continue
			}
			if len(textOut.Text) <= maxToolResultInlineBytes {
				continue
			}

			toolCallID := resultPart.ToolCallID
			savedPath, err := writeOverflowFile(dir, toolCallID, textOut.Text)
			if err != nil {
				log.Printf("Warning: failed to write overflow file for tool result %s: %v", toolCallID, err)
				continue
			}

			originalSize := len(textOut.Text)
			if toolResultHeadTailBytes*2 > originalSize {
				continue
			}
			truncated := headTailSummary(textOut.Text, toolResultHeadTailBytes, "truncated", savedPath)
			log.Printf("tool result %s truncated (%d bytes -> file %s)", toolCallID, originalSize, savedPath)

			resultPart.Output = fantasy.ToolResultOutputContentText{Text: truncated}
			messages[i].Content[j] = resultPart //nolint:gosec // j ranges over Content; in bounds by construction
		}
	}

	// Phase 1b: per-call cap on tool call inputs.
	truncateToolCallInputs(messages, dir, maxToolResultInlineBytes, "", false)

	// Phase 2: cumulative budget enforcement.
	totalBytes := estimateMessagesSize(messages)
	if totalBytes <= maxTotalContextBytes {
		return messages
	}
	log.Printf("context budget exceeded: %d bytes > %d bytes, applying aggressive truncation", totalBytes, maxTotalContextBytes)

	for i := range messages {
		if messages[i].Role != fantasy.MessageRoleTool {
			continue
		}
		for j := range messages[i].Content {
			part := messages[i].Content[j] //nolint:gosec // j ranges over Content; in bounds by construction
			resultPart, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
			if !ok {
				continue
			}
			textOut, ok := resultPart.Output.(fantasy.ToolResultOutputContentText)
			if !ok {
				continue
			}
			if len(textOut.Text) <= budgetToolResultSize {
				continue
			}

			savedPath, err := writeOverflowFileWithSuffix(dir, resultPart.ToolCallID, "-budget", textOut.Text)
			if err != nil {
				log.Printf("Warning: failed to write budget overflow file for %s: %v", resultPart.ToolCallID, err)
				continue
			}

			originalSize := len(textOut.Text)
			if budgetHeadTailBytes*2 >= originalSize {
				continue
			}
			truncated := headTailSummary(textOut.Text, budgetHeadTailBytes, "budget truncated", savedPath)
			log.Printf("tool result %s budget-truncated (%d bytes -> file %s)", resultPart.ToolCallID, originalSize, savedPath)

			resultPart.Output = fantasy.ToolResultOutputContentText{Text: truncated}
			messages[i].Content[j] = resultPart //nolint:gosec // j ranges over Content; in bounds by construction
		}
	}

	// Phase 2b: tool call input truncation under cumulative budget.
	truncateToolCallInputs(messages, dir, budgetToolCallInputSize, "-budget", true)

	return messages
}

// truncateToolCallInputs scans assistant messages for ToolCallPart
// entries whose `Input` JSON is over `cap`, writes the full input to
// an overflow file, and replaces the in-conversation Input with a
// small JSON sentinel. The sentinel is valid JSON so the provider's
// parse of the assistant message doesn't break.
//
// Why a sentinel instead of head/tail byte slicing: a tool call's
// Input field is structured JSON that the provider may parse (some
// providers re-validate tool_use blocks against the tool schema, and
// most cache the parsed args downstream). Cutting a base64 string in
// the middle leaves malformed JSON — a quoted string opened but never
// closed — which would break the message before the model ever sees
// the truncation. Replacing the whole Input with `{"_truncated": true,
// "_original_bytes": N, "_saved_to": "..."}` keeps the message well-
// formed and tells the model exactly what happened.
//
// The matched ToolResultPart is left alone — when the model looks at
// the conversation, the result of this tool call is still there. The
// model doesn't need its own emitted args echoed back; it remembers
// what it sent. The sentinel is only there to satisfy structural
// requirements (matching tool_use → tool_result pairing) without
// carrying the bytes.
//
// suffix and budgetMode are passed through to writeOverflowFile so
// Phase 1 and Phase 2 truncations save to distinguishable files
// (the budget pass produces "-budget" suffixed files).
func truncateToolCallInputs(messages []fantasy.Message, dir string, byteCap int, suffix string, budgetMode bool) {
	for i := range messages {
		if messages[i].Role != fantasy.MessageRoleAssistant {
			continue
		}
		for j := range messages[i].Content {
			part := messages[i].Content[j]
			callPart, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part)
			if !ok {
				continue
			}
			if len(callPart.Input) <= byteCap {
				continue
			}

			originalSize := len(callPart.Input)
			savedPath, err := writeOverflowFileWithSuffix(dir, callPart.ToolCallID, suffix, callPart.Input)
			if err != nil {
				log.Printf("Warning: failed to write overflow file for tool call %s: %v", callPart.ToolCallID, err)
				continue
			}

			sentinel := buildToolCallInputSentinel(originalSize, savedPath, callPart.ToolName, budgetMode)
			label := "truncated"
			if budgetMode {
				label = "budget-truncated"
			}
			log.Printf("tool call %s %s (%d bytes -> file %s)", callPart.ToolCallID, label, originalSize, savedPath)

			callPart.Input = sentinel
			messages[i].Content[j] = callPart
		}
	}
}

// buildToolCallInputSentinel returns the small valid-JSON object that
// replaces an oversized tool call's Input. The keys are underscored so
// they don't collide with whatever the original tool's schema used.
// The `_note` is written for the LLM, not the user — it's part of the
// conversation it'll see on the next turn, so it explains what
// happened in a way that pre-empts a retry of the same oversized call.
func buildToolCallInputSentinel(originalSize int, savedPath, toolName string, budgetMode bool) string {
	note := "Tool call arguments exceeded the inline size cap and were saved to disk. The tool already ran with the full original arguments — its result is in this conversation. Do NOT re-emit this tool call with the same large payload; if you need to call the tool again, use a path-based / blob-based alternative (e.g. fastio_upload_file for fast.io uploads) instead of inline base64."
	if budgetMode {
		note = "Tool call arguments were aggressively truncated under cumulative-budget enforcement (total context size blew the budget). The tool already ran with the full original arguments — its result is in this conversation. Do NOT re-emit this tool call; the next attempt would also be truncated. Switch to a path-based / blob-based alternative for any future similar work."
	}
	type sentinel struct {
		Truncated     bool   `json:"_truncated"`
		ToolName      string `json:"_tool_name,omitempty"`
		OriginalBytes int    `json:"_original_bytes"`
		SavedTo       string `json:"_saved_to"`
		Note          string `json:"_note"`
	}
	b, err := json.Marshal(sentinel{
		Truncated:     true,
		ToolName:      toolName,
		OriginalBytes: originalSize,
		SavedTo:       savedPath,
		Note:          note,
	})
	if err != nil {
		// Fall back to a hand-rolled string that's still valid JSON.
		// json.Marshal of a struct of plain types can't actually fail
		// at runtime, but the type system makes us handle it anyway.
		return fmt.Sprintf(`{"_truncated":true,"_original_bytes":%d,"_saved_to":%q}`, originalSize, savedPath)
	}
	return string(b)
}

// headTailSummary returns a head + truncation notice + tail summary.
// Assumes headTailLen*2 <= len(text); callers guard that.
func headTailSummary(text string, headTailLen int, label, savedPath string) string {
	omitted := len(text) - 2*headTailLen
	return fmt.Sprintf(
		"%s\n\n... [%s %d bytes, full output saved to %s] ...\n\n%s",
		text[:headTailLen],
		label,
		omitted,
		savedPath,
		text[len(text)-headTailLen:],
	)
}

// estimateMessagesSize returns a byte estimate of all text content across
// all message parts. Used for Phase 2 budget enforcement.
func estimateMessagesSize(messages []fantasy.Message) int {
	total := 0
	for i := range messages {
		for _, part := range messages[i].Content {
			switch p := part.(type) {
			case fantasy.TextPart:
				total += len(p.Text)
			case fantasy.ToolResultPart:
				if txt, ok := p.Output.(fantasy.ToolResultOutputContentText); ok {
					total += len(txt.Text)
				}
			case fantasy.ToolCallPart:
				total += len(p.Input)
			}
		}
	}
	return total
}

// overflowDirFromContext returns (and lazily creates) the directory used
// for oversized tool result overflow files, scoped to the conversation
// in ctx when available.
//
// Resolution order:
//  1. If ctx carries a conversation id (threaded by the agent harness),
//     return workspace/<convID>/.overflow/. The leading dot keeps these
//     files out of casual listings but still readable by the bash and
//     view_file tools if the model needs the full payload. They get
//     cleaned up with the conversation's workspace.
//  2. Otherwise, fall back to $TMPDIR/chat-overflow/ — covers tests,
//     direct tool invocations, and any path that doesn't thread a
//     conversation id.
func overflowDirFromContext(ctx context.Context) string {
	if convID := tools.ConversationIDFromContext(ctx); convID != "" {
		return overflowDirForConversation(convID)
	}
	return fallbackOverflowDir()
}

// overflowDirForConversation returns the per-conversation overflow dir,
// creating it (and the enclosing workspace) on demand. Exposed as a
// package-level helper so tests can target it without a context.
func overflowDirForConversation(convID string) string {
	workspace, err := tools.EnsureWorkspaceDir(convID)
	if err != nil {
		// EnsureWorkspaceDir returns the intended path even on error so
		// we can still try to write. A subsequent MkdirAll may succeed
		// (the earlier error might have been the best-effort symlink
		// step, not the dir itself).
		workspace = tools.WorkspaceDirForConversation(convID)
	}
	dir := filepath.Join(workspace, ".overflow")
	_ = os.MkdirAll(dir, 0o750)
	return dir
}

// fallbackOverflowDir returns the shared $TMPDIR-based overflow dir
// used when no conversation id is available.
func fallbackOverflowDir() string {
	d := filepath.Join(os.TempDir(), "chat-overflow")
	_ = os.MkdirAll(d, 0o750)
	return d
}

// writeOverflowFile writes the full tool result text to a file in dir
// and returns the path. The filename includes the tool call ID for
// traceability.
func writeOverflowFile(dir, toolCallID, content string) (string, error) {
	return writeOverflowFileWithSuffix(dir, toolCallID, "", content)
}

func writeOverflowFileWithSuffix(dir, toolCallID, suffix, content string) (string, error) {
	safeID := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, toolCallID)
	name := fmt.Sprintf("tool-result-%s%s.json", safeID, suffix)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return "", fmt.Errorf("write overflow file: %w", err)
	}
	return path, nil
}

// chainPrepareSteps composes multiple PrepareStep functions into one.
// Each step sees the messages produced by the previous step. A step that
// returns empty PrepareStepResult.Messages is treated as pass-through.
// Used to stack overflow truncation + prompt caching.
func chainPrepareSteps(steps ...fantasy.PrepareStepFunction) fantasy.PrepareStepFunction {
	nonNil := make([]fantasy.PrepareStepFunction, 0, len(steps))
	for _, s := range steps {
		if s != nil {
			nonNil = append(nonNil, s)
		}
	}
	if len(nonNil) == 0 {
		return nil
	}
	if len(nonNil) == 1 {
		return nonNil[0]
	}
	return func(ctx context.Context, opts fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
		msgs := opts.Messages
		var final fantasy.PrepareStepResult
		for _, step := range nonNil {
			stepCtx, out, err := step(ctx, fantasy.PrepareStepFunctionOptions{
				Model:      opts.Model,
				Steps:      opts.Steps,
				StepNumber: opts.StepNumber,
				Messages:   msgs,
			})
			if err != nil {
				return ctx, fantasy.PrepareStepResult{}, err
			}
			ctx = stepCtx
			if out.Messages != nil {
				msgs = out.Messages
			}
			// Preserve non-message fields from whichever step sets them.
			final = mergePrepareStepResult(final, out)
		}
		final.Messages = msgs
		return ctx, final, nil
	}
}

// mergePrepareStepResult combines non-Messages fields from b into a.
// Later steps win. Messages is handled by chainPrepareSteps directly.
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
