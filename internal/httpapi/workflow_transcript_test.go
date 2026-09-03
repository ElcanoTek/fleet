package httpapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// TestWorkflowTranscript_CarriesTheMethod is the difference between saving a
// workflow and saving a question: the tools the run used have to survive into
// the transcript, because they are what makes the saved entry reproducible.
func TestWorkflowTranscript_CarriesTheMethod(t *testing.T) {
	history := []agent.HistoryEntry{
		entry(t, "user", "text", agent.TextContent{Text: "forecast the achievable CPA"}),
		entry(t, "assistant", "reasoning", agent.ReasoningContent{Text: "internal deliberation"}),
		entry(t, "assistant", "tool_call", agent.ToolCallContent{Name: "bash", Input: `{"command":"unzip Nissan.zip"}`}),
		entry(t, "tool", "tool_result", agent.ToolResultContent{Name: "bash", Text: "inflated 412 files"}),
		entry(t, "assistant", "tool_call", agent.ToolCallContent{Name: "run_python", Input: `{"code":"df.groupby('channel')"}`}),
		entry(t, "tool", "tool_result", agent.ToolResultContent{Name: "run_python", Text: "CPA by channel"}),
		entry(t, "assistant", "text", agent.TextContent{Text: "A blended CPA of $41 is defensible."}),
	}

	got := workflowTranscriptFromHistory(history)

	for _, want := range []string{
		"User: forecast the achievable CPA",
		"[tool: bash]",
		"unzip Nissan.zip",
		"[tool: run_python]",
		"Assistant: A blended CPA of $41 is defensible.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("transcript missing %q:\n%s", want, got)
		}
	}
	// Reasoning is the model's private deliberation, not a step of the
	// workflow, and it is bulky. Successful tool RESULTS are the run's data,
	// not its method — the single biggest thing in a transcript.
	for _, unwanted := range []string{"internal deliberation", "inflated 412 files", "CPA by channel"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("transcript should not carry %q:\n%s", unwanted, got)
		}
	}
}

// TestWorkflowTranscript_CollapsesRepeatedTools keeps a long run affordable:
// "ran bash 40 times" is the signal; forty near-identical lines is the same
// signal at forty times the cost.
func TestWorkflowTranscript_CollapsesRepeatedTools(t *testing.T) {
	history := []agent.HistoryEntry{
		entry(t, "user", "text", agent.TextContent{Text: "profile the data"}),
	}
	for i := 0; i < 40; i++ {
		history = append(history,
			entry(t, "assistant", "tool_call", agent.ToolCallContent{Name: "bash", Input: `{"command":"head -5 part.csv"}`}),
			entry(t, "tool", "tool_result", agent.ToolResultContent{Name: "bash", Text: "rows"}),
		)
	}
	history = append(history, entry(t, "assistant", "tool_call", agent.ToolCallContent{Name: "write_file", Input: `{"path":"out.md"}`}))

	got := workflowTranscriptFromHistory(history)

	if !strings.Contains(got, "[tool: bash ×40]") {
		t.Errorf("expected a collapsed run with its count:\n%s", got)
	}
	if strings.Count(got, "[tool: bash") != 1 {
		t.Errorf("the run should render once, got %d lines:\n%s", strings.Count(got, "[tool: bash"), got)
	}
	// The tool AFTER the run must still appear — collapsing must not swallow
	// the next step of the procedure.
	if !strings.Contains(got, "[tool: write_file]") {
		t.Errorf("the step after a collapsed run was lost:\n%s", got)
	}
}

// TestWorkflowTranscript_KeepsErrors: what went wrong mid-run is exactly the
// kind of thing a template should warn the next person about.
func TestWorkflowTranscript_KeepsErrors(t *testing.T) {
	history := []agent.HistoryEntry{
		entry(t, "assistant", "tool_call", agent.ToolCallContent{Name: "bash", Input: `{"command":"cat big.csv"}`}),
		entry(t, "tool", "tool_result", agent.ToolResultContent{Name: "bash", Text: "UnicodeDecodeError: invalid start byte", IsErr: true}),
	}
	got := workflowTranscriptFromHistory(history)
	if !strings.Contains(got, "[tool error: UnicodeDecodeError: invalid start byte]") {
		t.Errorf("a failed call is a pitfall worth keeping:\n%s", got)
	}
}

// TestWorkflowTranscript_Bounds keeps one enormous turn or a pasted payload
// from crowding the rest of the run out of the budget.
func TestWorkflowTranscript_Bounds(t *testing.T) {
	huge := strings.Repeat("x", workflowTurnMaxRunes*3)
	history := []agent.HistoryEntry{
		entry(t, "user", "text", agent.TextContent{Text: huge}),
		entry(t, "assistant", "tool_call", agent.ToolCallContent{Name: "bash", Input: strings.Repeat("y", workflowToolInputMaxRunes*3)}),
		entry(t, "assistant", "text", agent.TextContent{Text: "done"}),
	}
	got := workflowTranscriptFromHistory(history)

	if strings.Contains(got, strings.Repeat("x", workflowTurnMaxRunes+1)) {
		t.Error("an over-long turn was not clamped")
	}
	if strings.Contains(got, strings.Repeat("y", workflowToolInputMaxRunes+1)) {
		t.Error("an over-long tool input was not clamped")
	}
	// Bounding must not cost the later turns.
	if !strings.Contains(got, "Assistant: done") {
		t.Errorf("clamping dropped a later turn:\n%s", got)
	}
}

// TestWorkflowTranscript_Degrades: an entry whose content will not decode is
// skipped, never fatal — the same best-effort posture as the exporters.
func TestWorkflowTranscript_Degrades(t *testing.T) {
	history := []agent.HistoryEntry{
		{Role: "assistant", Type: "text", Content: json.RawMessage(`not valid json`)},
		{Role: "assistant", Type: "tool_call", Content: json.RawMessage(`also not valid`)},
		entry(t, "user", "text", agent.TextContent{Text: "still here"}),
	}
	if got := workflowTranscriptFromHistory(history); !strings.Contains(got, "still here") {
		t.Errorf("a malformed entry should be skipped, not fatal:\n%s", got)
	}
	if got := workflowTranscriptFromHistory(nil); got != "" {
		t.Errorf("empty history should render empty, got %q", got)
	}
}
