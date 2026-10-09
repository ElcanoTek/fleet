package tools

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func runShowUI(t *testing.T, args string) fantasy.ToolResponse {
	t.Helper()
	resp, err := NewShowUITool().Run(context.Background(), fantasy.ToolCall{ID: "call_1", Name: ShowUIToolName, Input: args})
	if err != nil {
		t.Fatalf("Run returned a Go error (fatal to the loop): %v", err)
	}
	return resp
}

func TestShowUIInteractiveCardTellsModelToStop(t *testing.T) {
	resp := runShowUI(t, `{"title":"Pick","components":[{"type":"select","id":"s","options":["a","b"]}],"actions":[{"id":"go","label":"Go"}]}`)
	if resp.IsError {
		t.Fatalf("valid card refused: %s", resp.Content)
	}
	for _, want := range []string{"UI_DISPLAYED card_id=call_1", "End your turn", UISubmissionPrefix + " card=call_1 action=<action_id>"} {
		if !strings.Contains(resp.Content, want) {
			t.Errorf("result %q missing %q", resp.Content, want)
		}
	}
}

func TestShowUIDisplayOnlyCardLetsModelContinue(t *testing.T) {
	resp := runShowUI(t, `{"title":"Chart","components":[{"type":"chart","kind":"bar","labels":["a"],"series":[{"name":"s","values":[1]}]}]}`)
	if resp.IsError || !strings.Contains(resp.Content, "display-only") {
		t.Fatalf("got %+v", resp)
	}
}

func TestShowUIQuickRepliesAreNotASubmitForm(t *testing.T) {
	resp := runShowUI(t, `{"title":"Q","components":[{"type":"text","text":"?"}],"actions":[{"id":"a","label":"A","kind":"message","message":"A please"}],"replaces":"call_0"}`)
	if resp.IsError || strings.Contains(resp.Content, UISubmissionPrefix) || !strings.Contains(resp.Content, "replaces card call_0") ||
		!strings.Contains(resp.Content, UIReplyPrefix+" card=call_1 action=<action_id>") {
		t.Fatalf("got %+v", resp)
	}
}

func TestShowUIInvalidCardIsAToolErrorNotAGoError(t *testing.T) {
	resp := runShowUI(t, `{"title":"x","components":[{"type":"marquee"}]}`)
	if !resp.IsError || !strings.Contains(resp.Content, "UI_INVALID") || !strings.Contains(resp.Content, `unknown component type "marquee"`) {
		t.Fatalf("got %+v", resp)
	}
}

func TestShowUIIsInteractiveOnly(t *testing.T) {
	for _, tl := range ExcludeInteractiveOnly(DefaultTools()) {
		if tl.Info().Name == ShowUIToolName {
			t.Fatal("show_ui offered to headless runs")
		}
	}
	found := false
	for _, tl := range DefaultTools() {
		if tl.Info().Name == ShowUIToolName {
			found = true
		}
	}
	if !found {
		t.Fatal("show_ui missing from the interactive roster")
	}
}

// The description is the model's only catalog; every component the validator
// accepts must be named in it, or the model can never learn to use it.
func TestShowUIDescriptionNamesEveryComponent(t *testing.T) {
	var catalog []string
	raw := mustRead(t, "../genui/testdata/catalog.json")
	if err := json.Unmarshal(raw, &catalog); err != nil {
		t.Fatal(err)
	}
	for _, c := range catalog {
		if !strings.Contains(showUIDescription, c+" {") {
			t.Errorf("tool description does not document component %q", c)
		}
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The acknowledgement stays far below any tool-output ceiling: a result
// replaced by the truncation envelope would no longer start with
// UI_DISPLAYED, and the browser would draw no card.
func TestShowUIAcknowledgementIsSmall(t *testing.T) {
	title := strings.Repeat("t", 20000)
	replaces := strings.Repeat("r", 256)
	resp := runShowUI(t, `{"title":"`+title+`","replaces":"`+replaces+`","components":[{"type":"text","text":"x"}]}`)
	if resp.IsError {
		t.Fatalf("valid card refused: %s", resp.Content)
	}
	if !strings.HasPrefix(resp.Content, "UI_DISPLAYED") || len(resp.Content) > 2048 {
		t.Fatalf("acknowledgement is %d bytes: %.120q", len(resp.Content), resp.Content)
	}
	if resp := runShowUI(t, `{"title":"x","replaces":"`+strings.Repeat("r", 257)+`","components":[{"type":"text","text":"x"}]}`); !resp.IsError {
		t.Fatal("over-long replaces accepted")
	}
}

// A card whose input the loop would redact is refused: the browser draws the
// redacted copy, where distinct secret-like values collapse to one.
func TestShowUIRefusesRedactedCards(t *testing.T) {
	prev := ShowUIRedactor
	ShowUIRedactor = func(s string) string { return strings.ReplaceAll(s, "sk-secret", "[REDACTED]") }
	t.Cleanup(func() { ShowUIRedactor = prev })
	resp := runShowUI(t, `{"title":"Key","components":[{"type":"select","id":"k","options":["sk-secret1","sk-secret2"]}],"actions":[{"id":"go","label":"Go"}]}`)
	if !resp.IsError || !strings.Contains(resp.Content, "looks like a secret") {
		t.Fatalf("got %+v", resp)
	}
	if resp := runShowUI(t, `{"title":"Key","components":[{"type":"select","id":"k","options":["a","b"]}],"actions":[{"id":"go","label":"Go"}]}`); resp.IsError {
		t.Fatalf("clean card refused: %s", resp.Content)
	}
	// A JSON escape hides the secret from the wire text, not from the
	// browser, which shows the decoded value.
	escaped := runShowUI(t, `{"title":"Key","components":[{"type":"select","id":"k","options":["sk\u002dsecret1","b"]}],"actions":[{"id":"go","label":"Go"}]}`)
	if !escaped.IsError || !strings.Contains(escaped.Content, "looks like a secret") {
		t.Fatalf("escaped secret: %+v", escaped)
	}
}
