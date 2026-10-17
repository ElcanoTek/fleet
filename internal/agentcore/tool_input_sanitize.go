// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package agentcore

import (
	"context"
	"encoding/json"
	"log"

	"charm.land/fantasy"
	"charm.land/fantasy/jsonrepair"
)

// Tool-call inputs replayed to a provider must be JSON objects.
//
// A model can stream tool-call arguments that are not one: an array, a bare
// string or number, `null`, or JSON truncated in a way jsonrepair cannot turn
// into a valid call. fantasy marks such a call Invalid and answers it with an
// error tool result, but the assistant message it appends to the history keeps
// the raw Input, and the OpenAI-style adapters (openrouter, openai) send that
// string back verbatim as the call's arguments on the next request. Anthropic
// behind OpenRouter (Amazon Bedrock) rejects the whole request with 400
// "tool_use.input: Input should be a valid dictionary" — the turn dies on a
// call that had already been answered with an error. Production hit this on
// 2026-10-16 (claude-haiku-5.5). And because the session log stores the raw
// input, every later turn in that conversation replays the same bad part.
//
// So before any provider request the history is normalized: an input that
// decodes to a JSON object passes untouched; anything else is repaired once
// with jsonrepair and kept if that yields an object, else replaced with "{}".
// ToolCallID and ToolName are kept, so each call stays paired with its result
// (the error result fantasy already returned says what went wrong). Only the
// replayed copy changes; the executed call and its journal row are unaffected.

// emptyToolInput is the replacement for an input that cannot be made an object.
const emptyToolInput = "{}"

// NormalizeToolCallInput returns input unchanged when it is a JSON object, the
// jsonrepair result when that is one, and "{}" otherwise. changed reports
// whether the returned value differs from input.
func NormalizeToolCallInput(input string) (normalized string, changed bool) {
	if isJSONObject(input) {
		return input, false
	}
	if repaired, err := jsonrepair.RepairJSON(input); err == nil && isJSONObject(repaired) {
		return repaired, true
	}
	return emptyToolInput, true
}

// isJSONObject reports whether s decodes to a JSON object (not null, not an
// array or scalar).
func isJSONObject(s string) bool {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return false
	}
	_, ok := v.(map[string]any)
	return ok
}

// SanitizeToolCallInputs returns msgs with every tool-call part's Input
// normalized (NormalizeToolCallInput) and the number of parts rewritten. It is
// copy-on-write: when nothing changes msgs itself is returned, and a rewrite
// never mutates the caller's messages or their Content slices (the history is
// shared with the run's own bookkeeping).
func SanitizeToolCallInputs(msgs []fantasy.Message) ([]fantasy.Message, int) {
	var out []fantasy.Message
	fixed := 0
	for i, msg := range msgs {
		var content []fantasy.MessagePart
		for j, part := range msg.Content {
			replacement, ok := sanitizedToolCallPart(part)
			if !ok {
				continue
			}
			if content == nil {
				content = append([]fantasy.MessagePart(nil), msg.Content...)
			}
			content[j] = replacement
			fixed++
		}
		if content == nil {
			continue
		}
		if out == nil {
			out = append([]fantasy.Message(nil), msgs...)
		}
		out[i].Content = content
	}
	if out == nil {
		return msgs, 0
	}
	return out, fixed
}

// sanitizedToolCallPart returns a normalized copy of part when it is a tool
// call (by value or pointer) whose Input is not a JSON object.
func sanitizedToolCallPart(part fantasy.MessagePart) (fantasy.MessagePart, bool) {
	var tc fantasy.ToolCallPart
	switch p := part.(type) {
	case fantasy.ToolCallPart:
		tc = p
	case *fantasy.ToolCallPart:
		if p == nil {
			return nil, false
		}
		tc = *p
	default:
		return nil, false
	}
	normalized, changed := NormalizeToolCallInput(tc.Input)
	if !changed {
		return nil, false
	}
	log.Printf("agentcore: replaying tool call %s (%s) with a normalized input: the model's arguments were not a JSON object", tc.ToolCallID, tc.ToolName)
	tc.Input = normalized
	return tc, true
}

// toolCallInputSanitizeStep normalizes tool-call inputs in the history before
// every provider request. It belongs before the context-budget reducer and the
// prompt-cache markers, which must see the final message slice.
func toolCallInputSanitizeStep() fantasy.PrepareStepFunction {
	return func(ctx context.Context, opts fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
		msgs, fixed := SanitizeToolCallInputs(opts.Messages)
		if fixed == 0 {
			return ctx, fantasy.PrepareStepResult{}, nil
		}
		return ctx, fantasy.PrepareStepResult{Messages: msgs}, nil
	}
}

// ToolCallInputSanitizeStep is toolCallInputSanitizeStep for the prepare-step
// chains built outside agentcore (the interactive finalize retries).
func ToolCallInputSanitizeStep() fantasy.PrepareStepFunction {
	return toolCallInputSanitizeStep()
}
