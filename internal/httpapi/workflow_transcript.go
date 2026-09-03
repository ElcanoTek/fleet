package httpapi

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// Rendering a conversation for the save-as-workflow synthesizer.
//
// transcriptFromHistory (promote_task.go) keeps only the user/assistant TEXT
// turns, which is right for a recurring-task proposal: that synthesizer needs
// the ask, not the method. Saving a chat as a reusable WORKFLOW needs the
// opposite emphasis. What makes such an entry worth re-running is how the work
// was actually done — which tools the agent reached for, in what order, and
// where it hit trouble — and none of that lives in the text turns.
//
// The proportions are the argument: the conversation this was built against is
// 289 history entries, of which 11 are user turns and 110 are tool calls and
// results. Feeding the text turns alone hands the synthesizer under 4% of the
// run and asks it to describe a workflow it cannot see.

const (
	// One turn's text and one tool input are bounded so a single pasted
	// dataset or a giant reply cannot crowd the rest of the arc out of the
	// budget. Generous enough to keep a normal turn whole.
	workflowTurnMaxRunes = 1500
	// A tool input is a hint about HOW the tool was used ("unzip Nissan.zip",
	// a SQL query, a file path). The first line or two carries that; the rest
	// is usually a payload.
	workflowToolInputMaxRunes = 220
	// A tool error is a pitfall worth encoding into the template ("the CSV has
	// a BOM", "that path is read-only"), so errors are kept — briefly.
	workflowToolErrorMaxRunes = 200
)

// workflowTranscriptFromHistory renders a conversation as the workflow it was:
// the human's asks, the agent's answers, and the tool calls between them.
//
// Consecutive calls to the same tool are collapsed to one line with a count.
// "ran bash 40 times" is the reusable signal; forty near-identical lines is
// budget spent to say the same thing. Tool RESULTS are omitted except when the
// call failed — a result body is the run's data, not its method, and it is the
// single largest thing in a transcript.
func workflowTranscriptFromHistory(history []agent.HistoryEntry) string {
	var b strings.Builder

	// Collapsed run of the tool currently repeating.
	var runName, runInput string
	var runCount int
	flush := func() {
		if runCount == 0 {
			return
		}
		if runCount == 1 {
			fmt.Fprintf(&b, "[tool: %s]", runName)
		} else {
			fmt.Fprintf(&b, "[tool: %s ×%d]", runName, runCount)
		}
		if runInput != "" {
			fmt.Fprintf(&b, " %s", runInput)
		}
		b.WriteString("\n")
		runName, runInput, runCount = "", "", 0
	}

	for _, e := range history {
		switch e.Type {
		case "text":
			if e.Role != "user" && e.Role != "assistant" {
				continue
			}
			var c agent.TextContent
			if json.Unmarshal(e.Content, &c) != nil {
				continue
			}
			text := strings.TrimSpace(c.Text)
			if text == "" {
				continue
			}
			flush()
			label := "User"
			if e.Role == "assistant" {
				label = "Assistant"
			}
			fmt.Fprintf(&b, "%s: %s\n\n", label, clampRunes(text, workflowTurnMaxRunes))
		case entryTypeToolCallMD:
			var c agent.ToolCallContent
			if json.Unmarshal(e.Content, &c) != nil {
				continue
			}
			name := strings.TrimSpace(c.Name)
			if name == "" {
				continue
			}
			if name == runName {
				runCount++
				continue
			}
			flush()
			runName = name
			runCount = 1
			// Keep the FIRST input of a run: it shows how the tool was
			// invoked, and later calls in a run are usually variations.
			if input := strings.TrimSpace(c.Input); input != "" {
				runInput = clampRunes(collapseWhitespace(input), workflowToolInputMaxRunes)
			}
		case "tool_result":
			var c agent.ToolResultContent
			if json.Unmarshal(e.Content, &c) != nil || !c.IsErr {
				continue
			}
			text := strings.TrimSpace(c.Text)
			if text == "" {
				continue
			}
			flush()
			fmt.Fprintf(&b, "[tool error: %s]\n", clampRunes(collapseWhitespace(text), workflowToolErrorMaxRunes))
		}
	}
	flush()
	return strings.TrimSpace(b.String())
}

// clampRunes truncates on a rune boundary, marking that it did so. Counting
// runes (not bytes) keeps a multi-byte character from being cut in half.
func clampRunes(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}

// collapseWhitespace folds newlines and runs of spaces into single spaces, so
// a pretty-printed JSON tool input renders as one line instead of thirty.
func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
