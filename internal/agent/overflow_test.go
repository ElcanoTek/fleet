package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/tools"
)

// newTestOverflowDir returns a per-test scratch dir to use as the overflow
// target. Using t.TempDir keeps truncation-file assertions isolated from
// other tests and from any real workspace state on the dev box.
func newTestOverflowDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func TestTruncateToolResults_SmallResultUntouched(t *testing.T) {
	smallText := strings.Repeat("x", 100)
	messages := []fantasy.Message{
		{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{
				fantasy.ToolResultPart{
					ToolCallID: "call-small",
					Output:     fantasy.ToolResultOutputContentText{Text: smallText},
				},
			},
		},
	}
	result := truncateToolResults(messages, newTestOverflowDir(t))
	textOut := result[0].Content[0].(fantasy.ToolResultPart).Output.(fantasy.ToolResultOutputContentText)
	if textOut.Text != smallText {
		t.Error("small tool result should not be modified")
	}
}

func TestTruncateToolResults_LargeResultTruncated(t *testing.T) {
	dir := newTestOverflowDir(t)
	largeText := strings.Repeat("A", maxToolResultInlineBytes+1)
	messages := []fantasy.Message{
		{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{
				fantasy.ToolResultPart{
					ToolCallID: "call-large",
					Output:     fantasy.ToolResultOutputContentText{Text: largeText},
				},
			},
		},
	}
	result := truncateToolResults(messages, dir)
	textOut := result[0].Content[0].(fantasy.ToolResultPart).Output.(fantasy.ToolResultOutputContentText)

	if textOut.Text == largeText {
		t.Fatal("large tool result should have been truncated")
	}
	if !strings.Contains(textOut.Text, "truncated") {
		t.Error("truncated result should contain truncation notice")
	}
	if !strings.Contains(textOut.Text, dir) {
		t.Errorf("truncated result should reference the overflow dir %q, got %q", dir, textOut.Text)
	}
}

func TestTruncateToolResults_NonToolMessagesUntouched(t *testing.T) {
	userMsg := "hello from user"
	assistantMsg := "hello from assistant"
	messages := []fantasy.Message{
		{
			Role:    fantasy.MessageRoleUser,
			Content: []fantasy.MessagePart{fantasy.TextPart{Text: userMsg}},
		},
		{
			Role:    fantasy.MessageRoleAssistant,
			Content: []fantasy.MessagePart{fantasy.TextPart{Text: assistantMsg}},
		},
	}
	result := truncateToolResults(messages, newTestOverflowDir(t))
	if result[0].Content[0].(fantasy.TextPart).Text != userMsg {
		t.Error("user message should not be modified")
	}
	if result[1].Content[0].(fantasy.TextPart).Text != assistantMsg {
		t.Error("assistant message should not be modified")
	}
}

func TestTruncateToolResults_HeadAndTailPreserved(t *testing.T) {
	head := "HEAD_START"
	tail := "TAIL_END"
	middle := strings.Repeat("M", maxToolResultInlineBytes)
	largeText := head + middle + tail

	messages := []fantasy.Message{
		{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{
				fantasy.ToolResultPart{
					ToolCallID: "call-headtail",
					Output:     fantasy.ToolResultOutputContentText{Text: largeText},
				},
			},
		},
	}
	result := truncateToolResults(messages, newTestOverflowDir(t))
	textOut := result[0].Content[0].(fantasy.ToolResultPart).Output.(fantasy.ToolResultOutputContentText)

	if !strings.HasPrefix(textOut.Text, head) {
		t.Error("truncated result should start with original head")
	}
	if !strings.HasSuffix(textOut.Text, tail) {
		t.Error("truncated result should end with original tail")
	}
}

func TestTruncateToolResults_OverflowFileContent(t *testing.T) {
	dir := newTestOverflowDir(t)
	largeText := strings.Repeat("Z", maxToolResultInlineBytes+500)
	toolCallID := "call-overflow-chat-abc"

	messages := []fantasy.Message{
		{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{
				fantasy.ToolResultPart{
					ToolCallID: toolCallID,
					Output:     fantasy.ToolResultOutputContentText{Text: largeText},
				},
			},
		},
	}
	truncateToolResults(messages, dir)

	path := filepath.Join(dir, "tool-result-call-overflow-chat-abc.json")
	data, err := os.ReadFile(path) // path is derived from our sanitized id under a test-owned dir
	if err != nil {
		t.Fatalf("overflow file should exist at %s: %v", path, err)
	}
	if string(data) != largeText {
		t.Errorf("overflow file content mismatch: got %d bytes, want %d", len(data), len(largeText))
	}
}

func TestEstimateMessagesSize(t *testing.T) {
	messages := []fantasy.Message{
		{
			Role:    fantasy.MessageRoleUser,
			Content: []fantasy.MessagePart{fantasy.TextPart{Text: "hello"}},
		},
		{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{
				fantasy.ToolResultPart{
					ToolCallID: "call-1",
					Output:     fantasy.ToolResultOutputContentText{Text: strings.Repeat("x", 1000)},
				},
			},
		},
		{
			Role: fantasy.MessageRoleAssistant,
			Content: []fantasy.MessagePart{
				fantasy.ToolCallPart{ToolCallID: "c2", ToolName: "bash", Input: `{"command":"ls"}`},
			},
		},
	}
	got := estimateMessagesSize(messages)
	expected := len("hello") + 1000 + len(`{"command":"ls"}`)
	if got != expected {
		t.Fatalf("expected %d, got %d", expected, got)
	}
}

func TestTruncateToolResults_BudgetEnforcement(t *testing.T) {
	resultSize := maxToolResultInlineBytes - 1
	resultCount := maxTotalContextBytes/resultSize + 2

	var messages []fantasy.Message
	for i := 0; i < resultCount; i++ {
		messages = append(messages, fantasy.Message{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{
				fantasy.ToolResultPart{
					ToolCallID: fmt.Sprintf("call-budget-%d", i),
					Output:     fantasy.ToolResultOutputContentText{Text: strings.Repeat("B", resultSize)},
				},
			},
		})
	}

	result := truncateToolResults(messages, newTestOverflowDir(t))

	totalAfter := estimateMessagesSize(result)
	if totalAfter >= maxTotalContextBytes {
		t.Fatalf("expected total size under budget (%d bytes), got %d", maxTotalContextBytes, totalAfter)
	}

	truncatedCount := 0
	for _, msg := range result {
		textOut := msg.Content[0].(fantasy.ToolResultPart).Output.(fantasy.ToolResultOutputContentText)
		if strings.Contains(textOut.Text, "budget truncated") {
			truncatedCount++
		}
	}
	if truncatedCount == 0 {
		t.Fatal("expected at least one result to be budget-truncated")
	}
}

func TestOverflowDirFromContext_FallbackWithoutConversationID(t *testing.T) {
	got := overflowDirFromContext(context.Background())
	want := filepath.Join(os.TempDir(), "chat-overflow")
	if got != want {
		t.Errorf("without a conversation id, expected fallback %q, got %q", want, got)
	}
}

func TestOverflowDirFromContext_ScopedToConversationWorkspace(t *testing.T) {
	// Pin CHAT_WORKSPACE_ROOT to a test-owned dir so we can assert the
	// exact path and not pollute ./workspace in the repo.
	root := t.TempDir()
	t.Setenv("CHAT_WORKSPACE_ROOT", root)

	ctx := tools.WithConversationID(context.Background(), "conv-xyz-42")
	got := overflowDirFromContext(ctx)
	want := filepath.Join(root, "conv-xyz-42", ".overflow")
	if got != want {
		t.Errorf("expected overflow dir %q, got %q", want, got)
	}
	if info, err := os.Stat(got); err != nil || !info.IsDir() {
		t.Errorf("overflow dir should exist and be a directory: err=%v", err)
	}
}

func TestOverflowTruncationStep_WritesIntoConversationWorkspace(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CHAT_WORKSPACE_ROOT", root)

	ctx := tools.WithConversationID(context.Background(), "conv-abc")
	step := overflowTruncationStep()

	largeText := strings.Repeat("Q", maxToolResultInlineBytes+1)
	msgs := []fantasy.Message{
		{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{
				fantasy.ToolResultPart{
					ToolCallID: "call-scoped",
					Output:     fantasy.ToolResultOutputContentText{Text: largeText},
				},
			},
		},
	}
	_, out, err := step(ctx, fantasy.PrepareStepFunctionOptions{Messages: msgs})
	if err != nil {
		t.Fatalf("step returned error: %v", err)
	}
	got := out.Messages[0].Content[0].(fantasy.ToolResultPart).Output.(fantasy.ToolResultOutputContentText)
	expectedPath := filepath.Join(root, "conv-abc", ".overflow", "tool-result-call-scoped.json")
	if !strings.Contains(got.Text, expectedPath) {
		t.Errorf("inline summary should reference per-conversation path %q, got %q", expectedPath, got.Text)
	}
	if _, err := os.Stat(expectedPath); err != nil {
		t.Errorf("overflow file should exist at %s: %v", expectedPath, err)
	}
}

func TestOverflowTruncationStep_PassThroughForSmallResults(t *testing.T) {
	step := overflowTruncationStep()
	msgs := []fantasy.Message{
		{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{
				fantasy.ToolResultPart{
					ToolCallID: "call-tiny",
					Output:     fantasy.ToolResultOutputContentText{Text: "ok"},
				},
			},
		},
	}
	_, out, err := step(context.Background(), fantasy.PrepareStepFunctionOptions{Messages: msgs})
	if err != nil {
		t.Fatalf("step returned error: %v", err)
	}
	got := out.Messages[0].Content[0].(fantasy.ToolResultPart).Output.(fantasy.ToolResultOutputContentText)
	if got.Text != "ok" {
		t.Errorf("expected pass-through, got %q", got.Text)
	}
}

func TestChainPrepareSteps_OrderAndComposition(t *testing.T) {
	// Step A doubles the user text; Step B uppercases it. Final must
	// reflect both transformations in order.
	stepA := func(ctx context.Context, opts fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
		out := make([]fantasy.Message, len(opts.Messages))
		copy(out, opts.Messages)
		for i := range out {
			if p, ok := out[i].Content[0].(fantasy.TextPart); ok {
				out[i].Content = []fantasy.MessagePart{fantasy.TextPart{Text: p.Text + p.Text}}
			}
		}
		return ctx, fantasy.PrepareStepResult{Messages: out}, nil
	}
	stepB := func(ctx context.Context, opts fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
		out := make([]fantasy.Message, len(opts.Messages))
		copy(out, opts.Messages)
		for i := range out {
			if p, ok := out[i].Content[0].(fantasy.TextPart); ok {
				out[i].Content = []fantasy.MessagePart{fantasy.TextPart{Text: strings.ToUpper(p.Text)}}
			}
		}
		return ctx, fantasy.PrepareStepResult{Messages: out}, nil
	}

	chain := chainPrepareSteps(stepA, stepB)
	_, out, err := chain(context.Background(), fantasy.PrepareStepFunctionOptions{
		Messages: []fantasy.Message{
			{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "ab"}}},
		},
	})
	if err != nil {
		t.Fatalf("chain returned error: %v", err)
	}
	got := out.Messages[0].Content[0].(fantasy.TextPart).Text
	if got != "ABAB" {
		t.Errorf("expected ABAB (A then B), got %q", got)
	}
}

func TestChainPrepareSteps_PassThroughEmpty(t *testing.T) {
	// A step that returns empty Messages should act as pass-through.
	noop := func(ctx context.Context, _ fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
		return ctx, fantasy.PrepareStepResult{}, nil
	}
	mutate := func(ctx context.Context, opts fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
		out := make([]fantasy.Message, len(opts.Messages))
		copy(out, opts.Messages)
		for i := range out {
			if p, ok := out[i].Content[0].(fantasy.TextPart); ok {
				out[i].Content = []fantasy.MessagePart{fantasy.TextPart{Text: p.Text + "!"}}
			}
		}
		return ctx, fantasy.PrepareStepResult{Messages: out}, nil
	}

	chain := chainPrepareSteps(noop, mutate, noop)
	_, out, err := chain(context.Background(), fantasy.PrepareStepFunctionOptions{
		Messages: []fantasy.Message{
			{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "hi"}}},
		},
	})
	if err != nil {
		t.Fatalf("chain returned error: %v", err)
	}
	got := out.Messages[0].Content[0].(fantasy.TextPart).Text
	if got != "hi!" {
		t.Errorf("expected noop-mutate-noop to yield 'hi!', got %q", got)
	}
}

func TestChainPrepareSteps_PropagatesError(t *testing.T) {
	want := errors.New("boom")
	bad := func(ctx context.Context, _ fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
		return ctx, fantasy.PrepareStepResult{}, want
	}
	after := func(ctx context.Context, opts fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
		t.Fatal("step after an error should not run")
		return ctx, fantasy.PrepareStepResult{Messages: opts.Messages}, nil
	}
	chain := chainPrepareSteps(bad, after)
	_, _, err := chain(context.Background(), fantasy.PrepareStepFunctionOptions{})
	if !errors.Is(err, want) {
		t.Fatalf("expected %v, got %v", want, err)
	}
}

func TestChainPrepareSteps_NilsAndSingletons(t *testing.T) {
	if chainPrepareSteps() != nil {
		t.Error("chainPrepareSteps() with no args should be nil")
	}
	if chainPrepareSteps(nil, nil) != nil {
		t.Error("chainPrepareSteps(nil, nil) should be nil")
	}
	single := func(ctx context.Context, opts fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
		return ctx, fantasy.PrepareStepResult{Messages: opts.Messages}, nil
	}
	if chainPrepareSteps(nil, single, nil) == nil {
		t.Error("chainPrepareSteps with one real step should return non-nil")
	}
}

// ── ToolCallPart.Input truncation ───────────────────────────────────────
//
// The result-side truncation tests above cover the Output side of the
// transcript. These tests pin the Input side: a misbehaving model that
// emits a 200 KB content_base64 in a tool_call's arguments must NOT
// have that blob sit in conversation history forever. The Input is
// replaced with a JSON sentinel that's small, valid JSON, and tells
// the model where the original was saved.

func TestTruncateToolCallInputs_SmallInputUntouched(t *testing.T) {
	smallInput := `{"command":"ls -la"}`
	messages := []fantasy.Message{
		{
			Role: fantasy.MessageRoleAssistant,
			Content: []fantasy.MessagePart{
				fantasy.ToolCallPart{ToolCallID: "c1", ToolName: "bash", Input: smallInput},
			},
		},
	}
	result := truncateToolResults(messages, newTestOverflowDir(t))
	got := result[0].Content[0].(fantasy.ToolCallPart).Input
	if got != smallInput {
		t.Errorf("small tool call input should not be modified\n got %q\nwant %q", got, smallInput)
	}
}

// TestTruncateToolCallInputs_LargeInputReplacedWithSentinel is the
// headline behavior: a 200 KB inline base64 in the tool call's
// arguments gets swapped for a small JSON sentinel that points at the
// overflow file. The sentinel is valid JSON (provider won't choke on
// reparsing the assistant message) and names the saved file so the
// model can recover the bytes via view_file if it ever needs them.
func TestTruncateToolCallInputs_LargeInputReplacedWithSentinel(t *testing.T) {
	dir := newTestOverflowDir(t)
	huge := `{"content_base64":"` + strings.Repeat("A", maxToolResultInlineBytes+1) + `"}`
	const callID = "call-bigarg"
	messages := []fantasy.Message{
		{
			Role: fantasy.MessageRoleAssistant,
			Content: []fantasy.MessagePart{
				fantasy.ToolCallPart{ToolCallID: callID, ToolName: fastIOUploadToolName, Input: huge},
			},
		},
	}
	result := truncateToolResults(messages, dir)
	got := result[0].Content[0].(fantasy.ToolCallPart).Input

	if got == huge {
		t.Fatal("oversized tool call input should have been truncated")
	}
	// Must stay valid JSON — otherwise the provider's parse of the
	// assistant message breaks and the model never sees the truncation.
	var probe map[string]any
	if err := json.Unmarshal([]byte(got), &probe); err != nil {
		t.Fatalf("sentinel is not valid JSON: %v\n--- got ---\n%s", err, got)
	}
	for _, want := range []string{"_truncated", "_original_bytes", "_saved_to", "_note"} {
		if _, ok := probe[want]; !ok {
			t.Errorf("sentinel missing key %q\n--- got ---\n%s", want, got)
		}
	}
	if probe["_truncated"] != true {
		t.Errorf("sentinel _truncated should be true, got %v", probe["_truncated"])
	}
	if probe["_tool_name"] != fastIOUploadToolName {
		t.Errorf("sentinel should preserve tool name, got %v", probe["_tool_name"])
	}
	// The overflow file must actually exist on disk so a future
	// view_file can recover the original payload.
	savedPath, _ := probe["_saved_to"].(string)
	if savedPath == "" {
		t.Fatal("sentinel _saved_to is empty")
	}
	if !strings.HasPrefix(savedPath, dir) {
		t.Errorf("overflow path should live under the test dir %q, got %q", dir, savedPath)
	}
	body, err := os.ReadFile(savedPath)
	if err != nil {
		t.Fatalf("overflow file unreadable: %v", err)
	}
	if string(body) != huge {
		t.Error("overflow file should contain the original input verbatim")
	}
}

// TestTruncateToolCallInputs_OnlyAssistantMessagesScanned: tool call
// parts only live in assistant messages. A ToolCallPart accidentally
// placed under MessageRoleTool (shouldn't happen in production but
// could happen via test setup or a future fantasy refactor) must NOT
// be touched — the scan must be role-scoped.
func TestTruncateToolCallInputs_OnlyAssistantMessagesScanned(t *testing.T) {
	huge := strings.Repeat("X", maxToolResultInlineBytes+1)
	messages := []fantasy.Message{
		{
			Role: fantasy.MessageRoleTool, // wrong role; should be ignored
			Content: []fantasy.MessagePart{
				fantasy.ToolCallPart{ToolCallID: "c1", ToolName: "bash", Input: huge},
			},
		},
	}
	result := truncateToolResults(messages, newTestOverflowDir(t))
	got := result[0].Content[0].(fantasy.ToolCallPart).Input
	if got != huge {
		t.Error("tool call part under wrong role should not be touched (scan is role-scoped)")
	}
}

// TestTruncateToolCallInputs_BudgetMode: under cumulative-budget
// enforcement (many sub-cap calls together still over the 6 MB ceiling),
// even sub-128KB call inputs get aggressively truncated to the 2 KB
// budget cap. The sentinel's note must reflect budget-mode so the
// model knows the cause is cumulative, not a single large blob.
func TestTruncateToolCallInputs_BudgetMode(t *testing.T) {
	dir := newTestOverflowDir(t)
	// Build enough mid-sized calls to blow the cumulative budget.
	// Each one is under the Phase-1 cap but together they exceed
	// maxTotalContextBytes.
	const perCall = 100 * 1024
	count := maxTotalContextBytes/perCall + 2
	var messages []fantasy.Message
	for i := 0; i < count; i++ {
		messages = append(messages, fantasy.Message{
			Role: fantasy.MessageRoleAssistant,
			Content: []fantasy.MessagePart{
				fantasy.ToolCallPart{
					ToolCallID: fmt.Sprintf("call-budget-%d", i),
					ToolName:   "bash",
					Input:      `{"command":"` + strings.Repeat("x", perCall-15) + `"}`,
				},
			},
		})
	}
	result := truncateToolResults(messages, dir)

	totalAfter := estimateMessagesSize(result)
	if totalAfter >= maxTotalContextBytes {
		t.Errorf("expected total under budget %d, got %d", maxTotalContextBytes, totalAfter)
	}

	// Check at least one call was budget-truncated and the sentinel
	// note distinguishes budget mode from per-call mode.
	budgetSentinels := 0
	for _, m := range result {
		if m.Role != fantasy.MessageRoleAssistant {
			continue
		}
		for _, part := range m.Content {
			cp, ok := part.(fantasy.ToolCallPart)
			if !ok {
				continue
			}
			var probe map[string]any
			if err := json.Unmarshal([]byte(cp.Input), &probe); err != nil {
				continue
			}
			if probe["_truncated"] != true {
				continue
			}
			note, _ := probe["_note"].(string)
			if strings.Contains(note, "cumulative-budget") {
				budgetSentinels++
			}
		}
	}
	if budgetSentinels == 0 {
		t.Fatal("expected at least one tool call input to be budget-truncated under budget mode")
	}
}

// TestBuildToolCallInputSentinel_ValidJSONShape pins the exact shape of
// the sentinel so downstream consumers (the model, future log parsers,
// support tooling) can rely on the underscored keys staying stable.
func TestBuildToolCallInputSentinel_ValidJSONShape(t *testing.T) {
	got := buildToolCallInputSentinel(15293, "/workspace/conv-1/.overflow/tool-call-input-abc.json", fastIOUploadToolName, false)
	var probe map[string]any
	if err := json.Unmarshal([]byte(got), &probe); err != nil {
		t.Fatalf("sentinel is not valid JSON: %v", err)
	}
	if probe["_truncated"] != true {
		t.Errorf("_truncated = %v, want true", probe["_truncated"])
	}
	if probe["_original_bytes"].(float64) != 15293 {
		t.Errorf("_original_bytes = %v, want 15293", probe["_original_bytes"])
	}
	if probe["_saved_to"] != "/workspace/conv-1/.overflow/tool-call-input-abc.json" {
		t.Errorf("_saved_to mismatch, got %v", probe["_saved_to"])
	}
	if probe["_tool_name"] != fastIOUploadToolName {
		t.Errorf("_tool_name mismatch, got %v", probe["_tool_name"])
	}
	if note, _ := probe["_note"].(string); !strings.Contains(note, "fastio_upload_file") {
		t.Errorf("non-budget note should point at fastio_upload_file as the alternative, got %q", note)
	}
}
