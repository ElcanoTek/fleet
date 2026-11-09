package agent

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
)

// oneShotModel is a fantasy.LanguageModel that records the single call it
// receives (and the deadline it ran under) and answers with content, or err.
type oneShotModel struct {
	itMockModel
	slug     string
	content  []fantasy.Content
	err      error
	calls    int
	call     fantasy.Call
	deadline time.Time
}

func (m *oneShotModel) Model() string {
	if m.slug != "" {
		return m.slug
	}
	return "test/one-shot"
}

func (m *oneShotModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	m.calls++
	m.call = call
	m.deadline, _ = ctx.Deadline()
	if m.err != nil {
		return nil, m.err
	}
	return &fantasy.Response{
		Content:      m.content,
		FinishReason: fantasy.FinishReasonStop,
		Usage:        fantasy.Usage{InputTokens: 12, OutputTokens: 7},
	}, nil
}

// captureLog redirects the standard logger for the duration of the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prev); log.SetFlags(flags) })
	return &buf
}

// TestGenerateOnce pins the shape every one-shot helper used to hand-roll: the
// system prompt, ONE user message, the temperature and output budget on the
// wire, a deadline from Timeout, every text part concatenated (reasoning
// skipped), and the aux log line only for a labelled call.
func TestGenerateOnce(t *testing.T) {
	logs := captureLog(t)
	model := &oneShotModel{content: []fantasy.Content{
		fantasy.ReasoningContent{Text: "thinking"},
		fantasy.TextContent{Text: "first "},
		fantasy.TextContent{Text: "second"},
	}}
	start := time.Now()
	text, res, err := generateOnce(context.Background(), model, oneShotCall{
		System:      "be brief",
		Prompt:      "summarize this",
		Temperature: 0.3,
		MaxTokens:   321,
		Timeout:     40 * time.Second,
		AuxLabel:    "unit_test_label",
	})
	if err != nil {
		t.Fatal(err)
	}
	if text != "first second" {
		t.Errorf("text = %q, want every text part concatenated", text)
	}
	if res == nil || res.Response.Content.Text() != "first " {
		t.Errorf("res must carry the raw response (first text part %q)", res.Response.Content.Text())
	}
	if model.calls != 1 {
		t.Fatalf("Generate called %d times, want 1", model.calls)
	}
	var system, user []string
	for _, msg := range model.call.Prompt {
		switch msg.Role {
		case fantasy.MessageRoleSystem:
			system = append(system, msgTextOf(msg))
		case fantasy.MessageRoleUser:
			user = append(user, msgTextOf(msg))
		case fantasy.MessageRoleAssistant, fantasy.MessageRoleTool:
			t.Errorf("unexpected %s message in a one-shot call", msg.Role)
		}
	}
	if len(system) != 1 || system[0] != "be brief" {
		t.Errorf("system messages = %q", system)
	}
	if len(user) != 1 || user[0] != "summarize this" {
		t.Errorf("user messages = %q", user)
	}
	if model.call.Temperature == nil || *model.call.Temperature != 0.3 {
		t.Errorf("temperature = %v, want 0.3", model.call.Temperature)
	}
	if model.call.MaxOutputTokens == nil || *model.call.MaxOutputTokens != 321 {
		t.Errorf("max output tokens = %v, want 321", model.call.MaxOutputTokens)
	}
	if model.deadline.IsZero() || model.deadline.Before(start.Add(39*time.Second)) || model.deadline.After(time.Now().Add(40*time.Second)) {
		t.Errorf("deadline %v is not Timeout from the call", model.deadline)
	}
	if got := logs.String(); !strings.Contains(got, "aux model call label=unit_test_label model=test/one-shot prompt_tokens=12 completion_tokens=7") {
		t.Errorf("labelled call must be metered, log = %q", got)
	}

	// Unlabelled: no aux log line.
	logs.Reset()
	if _, _, err := generateOnce(context.Background(), model, oneShotCall{System: "s", Prompt: "p", MaxTokens: 10, Timeout: time.Second}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "aux model call") {
		t.Errorf("unlabelled call must not be metered, log = %q", logs.String())
	}

	// A provider error comes back as-is, with no result and no meter line.
	logs.Reset()
	boom := errors.New("provider down")
	failing := &oneShotModel{err: boom}
	text, res, err = generateOnce(context.Background(), failing, oneShotCall{System: "s", Prompt: "p", MaxTokens: 10, Timeout: time.Second, AuxLabel: "unit_test_label"})
	if !errors.Is(err, boom) || text != "" || res != nil {
		t.Errorf("failure = (%q, %v, %v), want (\"\", nil, provider down)", text, res, err)
	}
	if strings.Contains(logs.String(), "aux model call") {
		t.Errorf("a failed call is not metered, log = %q", logs.String())
	}

	// Gemini 4 Argon applies the 32K completion-token floor on one-shot aux
	// calls so thinking never consumes a small auxiliary token budget.
	gemini := &oneShotModel{
		slug:    "google/gemini-4-argon",
		content: []fantasy.Content{fantasy.TextContent{Text: "Title"}},
	}
	if _, _, err := generateOnce(context.Background(), gemini, oneShotCall{System: "s", Prompt: "p", MaxTokens: 512, Timeout: time.Second}); err != nil {
		t.Fatal(err)
	}
	if gemini.call.MaxOutputTokens == nil || *gemini.call.MaxOutputTokens != 32768 {
		t.Errorf("gemini-4-argon MaxOutputTokens = %v, want 32768", gemini.call.MaxOutputTokens)
	}
}
