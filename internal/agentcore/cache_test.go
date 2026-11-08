package agentcore

// Lifted+adapted from chat cache_test.go. The kill-switch env var is the
// canonical FLEET_DISABLE_PROMPT_CACHE (chat used CHAT_DISABLE_PROMPT_CACHE,
// which ADR-0106 removed). The `~`-alias handling that chat tested is
// preserved by supportsExplicitBreakpoints.

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
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
		// Provider-local slugs (#289 native providers): selectProvider strips
		// the "<provider-name>/" routing prefix, so the engine sees the bare
		// model id. These must still get breakpoints — a native-Anthropic run
		// without them pays full input price every turn.
		{"claude-fable-5", true},
		{"claude-opus-4.6", true},
		{"Claude-Sonnet-4.6", true}, // case-insensitive, same as the "/" forms
		{"gemini-3-flash-preview", true},
		// Bedrock's Claude ids (bedrock type): the model's own and an
		// inference profile's, which carry a geographic prefix.
		{"anthropic.claude-sonnet-4-5-20250929-v1:0", true},
		{"us.anthropic.claude-sonnet-4-5-20250929-v1:0", true},
		{"global.anthropic.claude-haiku-4-5-20251001-v1:0", true},
		{"amazon.nova-pro-v1:0", false},
		// Bare slugs of implicit-cache families stay unmarked.
		{"gpt-5.4", false},
		{"kimi-k2.5", false},
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
	msgs := []fantasy.Message{
		fantasy.NewSystemMessage("you are an assistant"),
		fantasy.NewUserMessage("hello"),
		newAssistantMessage("hi"),
		fantasy.NewUserMessage("how are you"),
		newAssistantMessage("good"),
		fantasy.NewUserMessage("bye"),
	}
	step := PromptCachingStep("anthropic/claude-sonnet-4.6")
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
	msgs := []fantasy.Message{
		fantasy.NewSystemMessage("you are an assistant"),
		fantasy.NewUserMessage("hello"),
	}
	// Gemini on OpenRouter makes the LAST breakpoint an explicit cache write,
	// so only the stable prefix is marked: a rolling tail marker would write a
	// fresh cache of the whole prompt on every request (stablePrefixOnly). The
	// fixtures keep the tail no larger than the prefix, where the marker stays
	// (TestPromptCachingStep_GeminiDropsTheMarkerOnceTheTailOutgrowsIt).
	for _, slug := range []string{"google/gemini-3-flash-preview", "google/gemini-4-argon", "~google/gemini-flash-latest", "gemini-3-flash-preview"} {
		step := PromptCachingStep(slug)
		_, out, err := step(context.Background(), fantasy.PrepareStepFunctionOptions{Messages: msgs})
		if err != nil {
			t.Fatalf("%s step: %v", slug, err)
		}
		if !hasCacheMarker(out.Messages[0]) {
			t.Errorf("%s: system message should carry a cache marker", slug)
		}
		if hasCacheMarker(out.Messages[1]) {
			t.Errorf("%s: the moving tail must not carry a cache marker", slug)
		}
	}

	// With a compaction summary, the summary is the last (and so the used)
	// breakpoint: stable until the next compaction, so it is read back too.
	summary := fantasy.NewUserMessage(compactionSummaryPrefix + "] dropped 12 messages")
	long := []fantasy.Message{fantasy.NewSystemMessage("sys"), summary, newAssistantMessage("a"), fantasy.NewUserMessage("b")}
	_, out, err := PromptCachingStep("google/gemini-4-argon", WithCompactionSummaryBreakpoint(nil))(
		context.Background(), fantasy.PrepareStepFunctionOptions{Messages: long})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []bool{true, true, false, false} {
		if hasCacheMarker(out.Messages[i]) != want {
			t.Errorf("gemini with summary: msg[%d] marker = %v, want %v", i, !want, want)
		}
	}
}

// Once the rest of the prompt outgrows the stable prefix, Gemini gets no
// breakpoint at all: an explicit cache would be read back only up to it, and
// Google's implicit cache, which reads most of the earlier prompt back, adds
// nothing past an explicit one. Anthropic keeps its breakpoints.
func TestPromptCachingStep_GeminiDropsTheMarkerOnceTheTailOutgrowsIt(t *testing.T) {
	t.Setenv("FLEET_DISABLE_PROMPT_CACHE", "")
	system := fantasy.NewSystemMessage(strings.Repeat("s", 4000))
	summary := fantasy.NewUserMessage(compactionSummaryPrefix + "] " + strings.Repeat("m", 2000))
	marked := func(model string, msgs []fantasy.Message, opts ...CacheOption) []bool {
		_, out, err := PromptCachingStep(model, append(opts, WithCompactionSummaryBreakpoint(nil))...)(
			context.Background(), fantasy.PrepareStepFunctionOptions{Messages: msgs})
		if err != nil {
			t.Fatal(err)
		}
		got := make([]bool, len(out.Messages))
		for i, m := range out.Messages {
			got[i] = hasCacheMarker(m)
		}
		return got
	}
	user := func(chars int) fantasy.Message { return fantasy.NewUserMessage(strings.Repeat("u", chars)) }
	for _, slug := range []string{"google/gemini-3.8-flash", "google/gemini-4-argon", "~google/gemini-flash-latest", "gemini-3-flash-preview"} {
		for _, tc := range []struct {
			name string
			msgs []fantasy.Message
			opts []CacheOption
			want []bool
		}{
			{"tail smaller than the system prompt", []fantasy.Message{system, user(3000)}, nil, []bool{true, false}},
			{"tail as large as the system prompt", []fantasy.Message{system, user(4000)}, nil, []bool{true, false}},
			{"tail larger than the system prompt", []fantasy.Message{system, user(5000)}, nil, []bool{false, false}},
			{"tail larger than the system prompt, smaller than it with the tools", []fantasy.Message{system, user(5000)}, []CacheOption{WithStablePrefixTokens(1000)}, []bool{true, false}},
			{"tail smaller than system and summary", []fantasy.Message{system, summary, user(5000)}, nil, []bool{true, true, false}},
			{"tail larger than system and summary", []fantasy.Message{system, summary, user(7000)}, nil, []bool{false, false, false}},
			// A screenshot counts at about what Gemini bills for it, not at
			// its base64 length (33K estimated tokens for this one).
			{"an image in the tail", []fantasy.Message{fantasy.NewSystemMessage(strings.Repeat("s", 8000)), {
				Role: fantasy.MessageRoleUser,
				Content: []fantasy.MessagePart{
					fantasy.TextPart{Text: "what is on this screen?"},
					fantasy.FilePart{Filename: "shot.png", MediaType: "image/png", Data: make([]byte, 100_000)},
				},
			}}, nil, []bool{true, false}},
			{"a screenshot tool result in the tail", []fantasy.Message{fantasy.NewSystemMessage(strings.Repeat("s", 8000)), {
				Role:    fantasy.MessageRoleAssistant,
				Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: "c1", ToolName: "screenshot", Input: `{}`}},
			}, {
				Role: fantasy.MessageRoleTool,
				Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "c1", Output: fantasy.ToolResultOutputContentMedia{
					Data: strings.Repeat("A", 130_000), MediaType: "image/png",
				}}},
			}}, nil, []bool{true, false, false}},
		} {
			if got := marked(slug, tc.msgs, tc.opts...); !slices.Equal(got, tc.want) {
				t.Errorf("%s, %s: markers = %v, want %v", slug, tc.name, got, tc.want)
			}
		}
	}
	big := []fantasy.Message{system, user(50_000)}
	if got := marked("anthropic/claude-haiku-5.5", big); !got[0] || !got[1] {
		t.Errorf("anthropic with a large tail: markers = %v, want the system prompt and the tail marked", got)
	}
}

func TestPromptCachingStep_OpenAIIsNoop(t *testing.T) {
	t.Setenv("FLEET_DISABLE_PROMPT_CACHE", "")
	msgs := []fantasy.Message{
		fantasy.NewSystemMessage("sys"),
		fantasy.NewUserMessage("hi"),
	}
	step := PromptCachingStep("openai/gpt-5.4-nano")
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
	step := PromptCachingStep("anthropic/claude-sonnet-4.6")
	_, out, err := step(context.Background(), fantasy.PrepareStepFunctionOptions{Messages: msgs})
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if out.Messages != nil {
		t.Errorf("kill-switch should short-circuit to pass-through, got %d msgs", len(out.Messages))
	}
}

// TestPromptCachingStep_KillSwitchIgnoresRemovedAlias asserts the removed
// CHAT_ spelling (ADR-0106) no longer toggles the kill-switch.
func TestPromptCachingStep_KillSwitchIgnoresRemovedAlias(t *testing.T) {
	t.Setenv("FLEET_DISABLE_PROMPT_CACHE", "")
	t.Setenv("CHAT_DISABLE_PROMPT_CACHE", "1")
	msgs := []fantasy.Message{
		fantasy.NewSystemMessage("sys"),
		fantasy.NewUserMessage("hi"),
	}
	step := PromptCachingStep("anthropic/claude-sonnet-4.6")
	_, out, _ := step(context.Background(), fantasy.PrepareStepFunctionOptions{Messages: msgs})
	if out.Messages == nil {
		t.Error("the removed CHAT_ kill-switch spelling must not short-circuit caching")
	}
}

func TestPromptCachingStep_DoesNotMutateInput(t *testing.T) {
	t.Setenv("FLEET_DISABLE_PROMPT_CACHE", "")
	msgs := []fantasy.Message{
		fantasy.NewSystemMessage("sys"),
		fantasy.NewUserMessage("hi"),
	}
	step := PromptCachingStep("anthropic/claude-sonnet-4.6")
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
	step := PromptCachingStep("anthropic/claude-sonnet-4.6")
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
	step := PromptCachingStep("anthropic/claude-sonnet-4.6", WithCompactionSummaryBreakpoint(nil))
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

// TestPromptCachingStep_SkipsBudgetWindDownNotice pins that the request-local
// wind-down notice (appended by budgetWindDownStep, different bytes every
// step) does not consume a rolling recency breakpoint: both rolling slots land
// on the stable history messages before it.
func TestPromptCachingStep_SkipsBudgetWindDownNotice(t *testing.T) {
	orch := newOrchestrationState(nil)
	orch.setCeilings(10.0, 0)
	orch.CostUSD = 9.0
	notice := fantasy.NewUserMessage(orch.checkBudgetWindDown(0.5).windDownNotice())
	if !isBudgetWindDownNotice(notice) {
		t.Fatal("notice not recognised by its prefix matcher")
	}
	msgs := []fantasy.Message{
		fantasy.NewSystemMessage("sys"),
		fantasy.NewUserMessage("u1"),
		newAssistantMessage("a1"),
		fantasy.NewUserMessage("u2"),
		notice,
	}
	step := PromptCachingStep("anthropic/claude-sonnet-4.6")
	_, res, err := step(context.Background(), fantasy.PrepareStepFunctionOptions{Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	out := res.Messages
	if hasCacheMarker(out[4]) {
		t.Error("wind-down notice carries a breakpoint; its bytes change every step so it can never hit")
	}
	if !hasCacheMarker(out[3]) || !hasCacheMarker(out[2]) {
		t.Errorf("rolling breakpoints should land on the two history messages before the notice: u2=%v a1=%v", hasCacheMarker(out[3]), hasCacheMarker(out[2]))
	}
	if !hasCacheMarker(out[0]) || hasCacheMarker(out[1]) {
		t.Errorf("system=%v u1=%v; want system marked, u1 unmarked", hasCacheMarker(out[0]), hasCacheMarker(out[1]))
	}
}

func TestPromptCachingStep_RuntimeTodayFourthBreakpoint(t *testing.T) {
	t.Setenv("FLEET_DISABLE_PROMPT_CACHE", "")
	rt1, _ := RuntimeDateMessage("2026-11-03")
	rt2, _ := RuntimeDateMessage("2026-11-03")
	msgs := []fantasy.Message{
		fantasy.NewSystemMessage("sys"),
		fantasy.NewUserMessage("u1"),
		rt1,
		newAssistantMessage("a1"),
		fantasy.NewUserMessage("u2"),
		rt2,
		{
			Role:    fantasy.MessageRoleAssistant,
			Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: "tc1", ToolName: "bash", Input: `{}`}},
		},
		{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{fantasy.ToolResultPart{
				ToolCallID: "tc1",
				Output:     fantasy.ToolResultOutputContentText{Text: "ok"},
			}},
		},
	}
	step := PromptCachingStep("anthropic/claude-opus-5.5", WithCompactionSummaryBreakpoint(nil))
	_, res, err := step(context.Background(), fantasy.PrepareStepFunctionOptions{Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	out := res.Messages
	// Expect 4 breakpoints: system (0), latest unmarked runtime_today rt2 (5),
	// and the last two messages (6, 7). Earlier runtime_today rt1 (2) stays unmarked.
	want := map[int]bool{0: true, 5: true, 6: true, 7: true}
	for i, m := range out {
		if hasCacheMarker(m) != want[i] {
			t.Errorf("msg[%d] (role=%s) marker=%v, want %v", i, m.Role, hasCacheMarker(m), want[i])
		}
	}
}

func TestPromptCachingStep_CoalescesMultiPartAssistantText(t *testing.T) {
	t.Setenv("FLEET_DISABLE_PROMPT_CACHE", "")
	msgs := []fantasy.Message{
		fantasy.NewSystemMessage("sys"),
		{
			Role: fantasy.MessageRoleAssistant,
			Content: []fantasy.MessagePart{
				fantasy.TextPart{Text: "first part"},
				fantasy.TextPart{Text: "second part"},
			},
		},
		fantasy.NewUserMessage("follow up"),
	}
	step := PromptCachingStep("anthropic/claude-opus-5.5")
	_, res, err := step(context.Background(), fantasy.PrepareStepFunctionOptions{Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	assistant := res.Messages[1]
	if !hasCacheMarker(assistant) {
		t.Fatal("assistant message should carry a cache marker")
	}
	if len(assistant.Content) != 1 {
		t.Fatalf("assistant content parts = %d, want 1 coalesced TextPart", len(assistant.Content))
	}
	tp, ok := fantasy.AsMessagePart[fantasy.TextPart](assistant.Content[0])
	if !ok || tp.Text != "first part\nsecond part" {
		t.Errorf("coalesced text = %q, want %q", tp.Text, "first part\nsecond part")
	}
}

func TestNormalizeOpenRouterCacheControl(t *testing.T) {
	raw := []byte(`{
		"model": "anthropic/claude-opus-5.5",
		"messages": [
			{"role": "system", "content": "sys", "cache_control": {"type": "ephemeral"}},
			{"role": "user", "content": "u1"},
			{"role": "assistant", "content": "a1", "cache_control": {"type": "ephemeral"}},
			{"role": "assistant", "content": "", "tool_calls": [{"id": "c1", "type": "function", "function": {"name": "bash", "arguments": "{}"}, "cache_control": {"type": "ephemeral"}}]},
			{"role": "tool", "tool_call_id": "c1", "content": "out", "cache_control": {"type": "ephemeral"}},
			{"role": "user", "content": [{"type": "text", "text": "look"}, {"type": "image_url", "image_url": {"url": "data:image/png;base64,AAAA"}}], "cache_control": {"type": "ephemeral"}},
			{"role": "user", "content": "u2", "cache_control": {"type": "ephemeral"}}
		]
	}`)
	got := normalizeOpenRouterCacheControl(raw)
	var parsed struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("unmarshal rewritten body: %v", err)
	}
	// 0: system keeps top-level cache_control and string content.
	if _, ok := parsed.Messages[0]["cache_control"]; !ok {
		t.Error("system message lost top-level cache_control")
	}
	// 1: unmarked user message normalized to content-part array without cache_control.
	var u1Parts []map[string]any
	if err := json.Unmarshal(parsed.Messages[1]["content"], &u1Parts); err != nil || len(u1Parts) != 1 || u1Parts[0]["text"] != "u1" {
		t.Errorf("u1 content = %s, want 1-element text block array", parsed.Messages[1]["content"])
	}
	// 2: marked assistant message moves cache_control inside content[0].
	if _, ok := parsed.Messages[2]["cache_control"]; ok {
		t.Error("marked assistant message should not keep top-level cache_control")
	}
	var a1Parts []map[string]any
	if err := json.Unmarshal(parsed.Messages[2]["content"], &a1Parts); err != nil || len(a1Parts) != 1 || a1Parts[0]["cache_control"] == nil {
		t.Errorf("a1 content = %s, want cache_control inside content[0]", parsed.Messages[2]["content"])
	}
	// 3: tool-call assistant message with empty string content stays string "".
	var tcContent string
	if err := json.Unmarshal(parsed.Messages[3]["content"], &tcContent); err != nil || tcContent != "" {
		t.Errorf("tool-call assistant content = %s, want empty string", parsed.Messages[3]["content"])
	}
	// 5: multi-part user message moves top-level cache_control onto last part.
	if _, ok := parsed.Messages[5]["cache_control"]; ok {
		t.Error("multi-part user message should not keep top-level cache_control")
	}
	var multiParts []map[string]any
	if err := json.Unmarshal(parsed.Messages[5]["content"], &multiParts); err != nil || len(multiParts) != 2 || multiParts[1]["cache_control"] == nil {
		t.Errorf("multi-part user content = %s, want cache_control on last part", parsed.Messages[5]["content"])
	}
	// 6: marked user message moves cache_control inside content[0].
	if _, ok := parsed.Messages[6]["cache_control"]; ok {
		t.Error("u2 should not keep top-level cache_control")
	}
	var u2Parts []map[string]any
	if err := json.Unmarshal(parsed.Messages[6]["content"], &u2Parts); err != nil || len(u2Parts) != 1 || u2Parts[0]["cache_control"] == nil {
		t.Errorf("u2 content = %s, want cache_control inside content[0]", parsed.Messages[6]["content"])
	}

	// Implicit-cache family (openai/) must be returned byte-for-byte unchanged.
	implicitRaw := []byte(`{"model":"openai/gpt-6-luna-pro","messages":[{"role":"user","content":"hello"}]}`)
	if gotImplicit := normalizeOpenRouterCacheControl(implicitRaw); !slices.Equal(gotImplicit, implicitRaw) {
		t.Errorf("implicit-cache body was modified: %s", gotImplicit)
	}
}
