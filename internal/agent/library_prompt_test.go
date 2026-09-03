package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/structuredoutput"
)

// TestLibraryPromptSchema pins the contract the synthesizer's output is
// validated against: all three fields required, nothing else allowed.
func TestLibraryPromptSchema(t *testing.T) {
	valid := `{"name":"Campaign CPA forecast","description":"Forecasts an achievable CPA","content":"**Objective**\n..."}`
	if _, err := structuredoutput.ValidateOutput(valid, json.RawMessage(libraryPromptSchema)); err != nil {
		t.Fatalf("a conforming draft was rejected: %v", err)
	}
	for name, bad := range map[string]string{
		"missing content": `{"name":"n","description":"d"}`,
		"extra field":     `{"name":"n","description":"d","content":"c","cron":"0 9 * * *"}`,
	} {
		if _, err := structuredoutput.ValidateOutput(bad, json.RawMessage(libraryPromptSchema)); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

// TestKeepTranscriptEnds is the difference between a workflow template and a
// half of one. The recurring-task path keeps only the tail, because the
// refined ask lives at the end of an exploration. A workflow's OPENING turns
// carry its objective and its inputs — the template's first two sections — so
// truncation here has to eat the middle, which is the repetitive part.
func TestKeepTranscriptEnds(t *testing.T) {
	if got := keepTranscriptEnds("short", 100); got != "short" {
		t.Errorf("under budget should pass through unchanged, got %q", got)
	}

	head := strings.Repeat("A", 600)
	middle := strings.Repeat("M", 5000)
	tail := strings.Repeat("Z", 600)
	got := keepTranscriptEnds(head+middle+tail, 1500)

	if len([]rune(got)) > 1500+len([]rune("\n\n…[middle of the conversation omitted]…\n\n")) {
		t.Errorf("result exceeds the budget: %d runes", len([]rune(got)))
	}
	if !strings.HasPrefix(got, "AAAA") {
		t.Errorf("the opening — objective and inputs — was dropped:\n%.80s", got)
	}
	if !strings.HasSuffix(got, "ZZZZ") {
		t.Error("the close — the refined method and result — was dropped")
	}
	if !strings.Contains(got, "middle of the conversation omitted") {
		t.Error("truncation should say it happened")
	}
	if strings.Contains(got, strings.Repeat("M", 4000)) {
		t.Error("the middle should be what gets dropped")
	}
}

// TestTrimmedNames keeps a malformed connector list from padding the prompt.
func TestTrimmedNames(t *testing.T) {
	got := trimmedNames([]string{" gmail ", "", "   ", strings.Repeat("x", 200)})
	if len(got) != 2 {
		t.Fatalf("got %d names, want 2 (blanks dropped): %q", len(got), got)
	}
	if got[0] != "gmail" {
		t.Errorf("names should be trimmed, got %q", got[0])
	}
	// truncate() bounds the CONTENT at 80 and appends its own marker, so the
	// assertion is on the content plus that marker — not on 80 flat.
	if !strings.HasSuffix(got[1], "…[truncated]") {
		t.Errorf("a long name should be truncated with its marker, got %q", got[1])
	}
	if len(strings.TrimSuffix(got[1], "…[truncated]")) != 80 {
		t.Errorf("a long name should be bounded at 80 chars of content, got %d", len(strings.TrimSuffix(got[1], "…[truncated]")))
	}
}
