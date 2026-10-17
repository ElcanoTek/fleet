// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package agentcore

import (
	"context"
	"testing"

	"charm.land/fantasy"
)

func TestNormalizeToolCallInput(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		changed bool
	}{
		{"valid object untouched", `{"command":"ls","n":1}`, `{"command":"ls","n":1}`, false},
		{"empty object untouched", `{}`, `{}`, false},
		{"empty string", ``, `{}`, true},
		{"array", `[1]`, `{}`, true},
		{"bare string", `"s"`, `{}`, true},
		{"number", `123`, `{}`, true},
		{"null", `null`, `{}`, true},
		{"garbage", `not json at all`, `{}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := NormalizeToolCallInput(tc.in)
			if got != tc.want || changed != tc.changed {
				t.Fatalf("NormalizeToolCallInput(%q) = (%q, %v), want (%q, %v)", tc.in, got, changed, tc.want, tc.changed)
			}
		})
	}
}

// A truncated object is repaired rather than dropped: whatever jsonrepair
// recovers is kept when it is an object, and the result is always one.
func TestNormalizeToolCallInput_TruncatedObjectIsRepaired(t *testing.T) {
	got, changed := NormalizeToolCallInput(`{"command": "ls -la", "timeout": 3`)
	if !changed {
		t.Fatal("truncated input reported unchanged")
	}
	if !isJSONObject(got) {
		t.Fatalf("normalized input %q is not a JSON object", got)
	}
	if got == emptyToolInput {
		t.Fatalf("truncated object was dropped to {} instead of repaired")
	}
}

func TestSanitizeToolCallInputs_CopyOnWrite(t *testing.T) {
	bad := fantasy.ToolCallPart{ToolCallID: "c1", ToolName: "bash", Input: `[1]`}
	good := fantasy.ToolCallPart{ToolCallID: "c2", ToolName: "bash", Input: `{"command":"ls"}`}
	msgs := []fantasy.Message{
		fantasy.NewUserMessage("hi"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "running"}, bad, &good}},
	}
	out, fixed := SanitizeToolCallInputs(msgs)
	if fixed != 1 {
		t.Fatalf("fixed = %d, want 1", fixed)
	}
	tc, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](out[1].Content[1])
	if !ok || tc.Input != emptyToolInput || tc.ToolCallID != "c1" || tc.ToolName != "bash" {
		t.Fatalf("sanitized part = %+v, want c1/bash with {}", tc)
	}
	if got := out[1].Content[2]; got != fantasy.MessagePart(&good) {
		t.Fatalf("valid part was rewritten: %#v", got)
	}
	// The caller's history is untouched.
	orig, _ := fantasy.AsMessagePart[fantasy.ToolCallPart](msgs[1].Content[1])
	if orig.Input != `[1]` {
		t.Fatalf("input slice mutated: %q", orig.Input)
	}
}

func TestSanitizeToolCallInputs_NoChangeReturnsSameSlice(t *testing.T) {
	msgs := []fantasy.Message{{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
		fantasy.ToolCallPart{ToolCallID: "c1", ToolName: "bash", Input: `{"command":"ls"}`},
	}}}
	out, fixed := SanitizeToolCallInputs(msgs)
	if fixed != 0 || &out[0] != &msgs[0] {
		t.Fatalf("clean history was copied or reported fixed (fixed=%d)", fixed)
	}
}

func TestToolCallInputSanitizeStep(t *testing.T) {
	step := toolCallInputSanitizeStep()
	msgs := []fantasy.Message{{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
		fantasy.ToolCallPart{ToolCallID: "c1", ToolName: "bash", Input: `null`},
	}}}
	_, res, err := step(context.Background(), fantasy.PrepareStepFunctionOptions{Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	if res.Messages == nil {
		t.Fatal("step returned no messages for a history needing a fix")
	}
	tc, _ := fantasy.AsMessagePart[fantasy.ToolCallPart](res.Messages[0].Content[0])
	if tc.Input != emptyToolInput {
		t.Fatalf("step left input %q", tc.Input)
	}

	clean := []fantasy.Message{fantasy.NewUserMessage("hi")}
	_, res, err = step(context.Background(), fantasy.PrepareStepFunctionOptions{Messages: clean})
	if err != nil || res.Messages != nil {
		t.Fatalf("clean history: messages=%v err=%v, want nil/nil", res.Messages, err)
	}
}
