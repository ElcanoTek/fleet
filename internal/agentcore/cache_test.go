package agentcore

// Lifted+adapted from chat cache_test.go. The kill-switch env var is the
// canonical FLEET_DISABLE_PROMPT_CACHE (chat used CHAT_DISABLE_PROMPT_CACHE; the
// back-compat alias still works). The `~`-alias handling that chat tested is
// preserved by supportsExplicitBreakpoints.

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
)

func TestSupportsExplicitBreakpoints(t *testing.T) {
	cases := []struct {
		slug string
		want bool
	}{
		{"anthropic/claude-sonnet-4.6", true},
		{"anthropic/claude-4.6-opus", true},
		{"Anthropic/claude-sonnet-4.6", true},   // case-insensitive prefix match
		{" anthropic/claude-sonnet-4.6", true},  // trims surrounding whitespace
		{"anthropic/claude-sonnet-4.6\n", true}, // trailing newline trimmed
		{"google/gemini-3-flash-preview", true},
		{"Google/gemini-2.5-pro", true},
		// OpenRouter floating aliases — the leading `~` must be stripped before
		// prefix matching so the alias inherits the lab's caching policy.
		{"~anthropic/claude-sonnet-latest", true},
		{"~google/gemini-flash-latest", true},
		// Implicit-cache families — no explicit breakpoints.
		{"openai/gpt-5.4", false},
		{"openai/gpt-5.4-nano", false},
		{"deepseek/deepseek-v3.1", false},
		{"x-ai/grok-4", false},
		{"moonshotai/kimi-k2.5", false},
		{"moonshotai/kimi-k2.6:nitro", false},
		{"~moonshotai/kimi-latest", false},
		{"qwen/qwen3-max", false},
		{"z-ai/glm-4.6", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := supportsExplicitBreakpoints(tc.slug); got != tc.want {
			t.Errorf("supportsExplicitBreakpoints(%q) = %v, want %v", tc.slug, got, tc.want)
		}
	}
}

func TestCacheControlOptions_Shape(t *testing.T) {
	opts := cacheControlOptions()
	raw, ok := opts[anthropic.Name]
	if !ok {
		t.Fatalf("missing anthropic.Name namespace: %+v", opts)
	}
	cc, ok := raw.(*anthropic.ProviderCacheControlOptions)
	if !ok {
		t.Fatalf("wrong type: %T", raw)
	}
	if cc.CacheControl.Type != "ephemeral" {
		t.Errorf("CacheControl.Type = %q, want ephemeral", cc.CacheControl.Type)
	}
}

func newAssistantMessage(text string) fantasy.Message {
	return fantasy.Message{
		Role:    fantasy.MessageRoleAssistant,
		Content: []fantasy.MessagePart{fantasy.TextPart{Text: text}},
	}
}

// hasCacheMarker reports whether msg carries an ephemeral cache_control marker.
func hasCacheMarker(msg fantasy.Message) bool {
	if msg.ProviderOptions == nil {
		return false
	}
	cc := anthropic.GetCacheControl(msg.ProviderOptions)
	return cc != nil && cc.Type == "ephemeral"
}

func TestPromptCachingStep_AnthropicPlacesBreakpoints(t *testing.T) {
	t.Setenv("FLEET_DISABLE_PROMPT_CACHE", "")
	t.Setenv("CHAT_DISABLE_PROMPT_CACHE", "")
	t.Setenv("CUTLASS_DISABLE_PROMPT_CACHE", "")
	msgs := []fantasy.Message{
		fantasy.NewSystemMessage("you are an assistant"),
		fantasy.NewUserMessage("hello"),
		newAssistantMessage("hi"),
		fantasy.NewUserMessage("how are you"),
		newAssistantMessage("good"),
		fantasy.NewUserMessage("bye"),
	}
	step := promptCachingStep("anthropic/claude-sonnet-4.6")
	_, out, err := step(context.Background(), fantasy.PrepareStepFunctionOptions{Messages: msgs})
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if len(out.Messages) != len(msgs) {
		t.Fatalf("Messages length changed: got %d want %d", len(out.Messages), len(msgs))
	}

	marked := map[int]bool{}
	for i, m := range out.Messages {
		if hasCacheMarker(m) {
			marked[i] = true
		}
	}
	want := map[int]bool{0: true, 4: true, 5: true}
	if len(marked) != len(want) {
		t.Errorf("marked indices = %v, want %v", marked, want)
	}
	for i := range want {
		if !marked[i] {
			t.Errorf("expected marker on msg[%d] (role=%s)", i, out.Messages[i].Role)
		}
	}
}

func TestPromptCachingStep_GooglePlacesBreakpoints(t *testing.T) {
	t.Setenv("FLEET_DISABLE_PROMPT_CACHE", "")
	t.Setenv("CHAT_DISABLE_PROMPT_CACHE", "")
	t.Setenv("CUTLASS_DISABLE_PROMPT_CACHE", "")
	msgs := []fantasy.Message{
		fantasy.NewSystemMessage("you are an assistant"),
		fantasy.NewUserMessage("hello"),
	}
	step := promptCachingStep("google/gemini-3-flash-preview")
	_, out, err := step(context.Background(), fantasy.PrepareStepFunctionOptions{Messages: msgs})
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if !hasCacheMarker(out.Messages[0]) {
		t.Error("system message should carry a cache marker for google/")
	}
	if !hasCacheMarker(out.Messages[1]) {
		t.Error("last user message should carry a cache marker for google/")
	}
}

func TestPromptCachingStep_OpenAIIsNoop(t *testing.T) {
	t.Setenv("FLEET_DISABLE_PROMPT_CACHE", "")
	msgs := []fantasy.Message{
		fantasy.NewSystemMessage("sys"),
		fantasy.NewUserMessage("hi"),
	}
	step := promptCachingStep("openai/gpt-5.4-nano")
	_, out, err := step(context.Background(), fantasy.PrepareStepFunctionOptions{Messages: msgs})
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if out.Messages != nil {
		t.Errorf("openai path should return empty Messages (pass-through), got %d", len(out.Messages))
	}
}

func TestPromptCachingStep_KillSwitch(t *testing.T) {
	t.Setenv("FLEET_DISABLE_PROMPT_CACHE", "1")
	msgs := []fantasy.Message{
		fantasy.NewSystemMessage("sys"),
		fantasy.NewUserMessage("hi"),
	}
	step := promptCachingStep("anthropic/claude-sonnet-4.6")
	_, out, err := step(context.Background(), fantasy.PrepareStepFunctionOptions{Messages: msgs})
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if out.Messages != nil {
		t.Errorf("kill-switch should short-circuit to pass-through, got %d msgs", len(out.Messages))
	}
}

// TestPromptCachingStep_KillSwitchLegacyAlias asserts the CHAT_/CUTLASS_
// back-compat aliases still toggle the kill-switch.
func TestPromptCachingStep_KillSwitchLegacyAlias(t *testing.T) {
	t.Setenv("FLEET_DISABLE_PROMPT_CACHE", "")
	t.Setenv("CHAT_DISABLE_PROMPT_CACHE", "1")
	msgs := []fantasy.Message{
		fantasy.NewSystemMessage("sys"),
		fantasy.NewUserMessage("hi"),
	}
	step := promptCachingStep("anthropic/claude-sonnet-4.6")
	_, out, _ := step(context.Background(), fantasy.PrepareStepFunctionOptions{Messages: msgs})
	if out.Messages != nil {
		t.Errorf("legacy CHAT_ kill-switch should short-circuit, got %d msgs", len(out.Messages))
	}
}

func TestPromptCachingStep_DoesNotMutateInput(t *testing.T) {
	t.Setenv("FLEET_DISABLE_PROMPT_CACHE", "")
	msgs := []fantasy.Message{
		fantasy.NewSystemMessage("sys"),
		fantasy.NewUserMessage("hi"),
	}
	step := promptCachingStep("anthropic/claude-sonnet-4.6")
	if _, _, err := step(context.Background(), fantasy.PrepareStepFunctionOptions{Messages: msgs}); err != nil {
		t.Fatalf("step: %v", err)
	}
	for i, m := range msgs {
		if m.ProviderOptions != nil {
			t.Errorf("input msg[%d] ProviderOptions was mutated: %+v", i, m.ProviderOptions)
		}
	}
}

func TestPromptCachingStep_OnlyOneBreakpointOnShortHistory(t *testing.T) {
	t.Setenv("FLEET_DISABLE_PROMPT_CACHE", "")
	msgs := []fantasy.Message{
		fantasy.NewSystemMessage("sys"),
		fantasy.NewUserMessage("hi"),
	}
	step := promptCachingStep("anthropic/claude-sonnet-4.6")
	_, out, err := step(context.Background(), fantasy.PrepareStepFunctionOptions{Messages: msgs})
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	count := 0
	for _, m := range out.Messages {
		if hasCacheMarker(m) {
			count++
		}
	}
	if count != 2 {
		t.Errorf("want 2 markers (system + user), got %d", count)
	}
}

// TestPromptCachingStep_CompactionSummaryBreakpoint exercises cutlass's additive
// 4th breakpoint: with the option enabled, a compaction-summary message also
// gets a marker (system + summary + last two non-system = up to 4).
func TestPromptCachingStep_CompactionSummaryBreakpoint(t *testing.T) {
	t.Setenv("FLEET_DISABLE_PROMPT_CACHE", "")
	summary := fantasy.NewUserMessage(compactionSummaryPrefix + "] dropped 12 messages")
	msgs := []fantasy.Message{
		fantasy.NewSystemMessage("sys"),
		summary,
		newAssistantMessage("a"),
		fantasy.NewUserMessage("b"),
		newAssistantMessage("c"),
	}
	step := promptCachingStep("anthropic/claude-sonnet-4.6", WithCompactionSummaryBreakpoint(nil))
	_, out, err := step(context.Background(), fantasy.PrepareStepFunctionOptions{Messages: msgs})
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if !hasCacheMarker(out.Messages[1]) {
		t.Error("compaction summary should carry a cache marker when the option is set")
	}
	count := 0
	for _, m := range out.Messages {
		if hasCacheMarker(m) {
			count++
		}
	}
	if count != 4 {
		t.Errorf("want 4 markers (system + summary + last two non-system), got %d", count)
	}
}
