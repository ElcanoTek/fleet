package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/structuredoutput"
)

// LibraryPromptDraft is a synthesized suggestion for saving a chat
// conversation as a reusable prompt-library entry. Content is a clean,
// self-contained restatement of the useful ask (no references to "this
// chat"), Name a short label, and Description a one-line summary shown in
// the library list. The user reviews and edits the draft before saving —
// nothing is persisted by the synthesizer itself.
type LibraryPromptDraft struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Content     string `json:"content"`
}

// libraryPromptSchema constrains the synthesizer's output. draft-07,
// object-only with additionalProperties:false so the model can't pad it;
// all three fields are required (a library entry needs each of them).
const libraryPromptSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["name", "description", "content"],
  "properties": {
    "name": { "type": "string" },
    "description": { "type": "string" },
    "content": { "type": "string" }
  }
}`

// SuggestLibraryPrompt distills a conversation transcript into a proposed
// prompt-library entry: a clean standalone prompt, a short name, and a
// one-line description. It mirrors SuggestRecurringTask — a short-lived
// fantasy.NewAgent call through the host-side resolver against
// config.LibraryPromptModel, with structured-output validation and a hard
// timeout — and likewise returns an error on failure, because it backs a
// user-initiated action that should report "couldn't synthesize" rather
// than silently do nothing.
func (m *Manager) SuggestLibraryPrompt(ctx context.Context, transcript string) (*LibraryPromptDraft, error) {
	if strings.TrimSpace(transcript) == "" {
		return nil, fmt.Errorf("empty transcript")
	}
	model, err := m.modelResolver().Resolve(ctx, m.config.LibraryPromptModel)
	if err != nil {
		return nil, fmt.Errorf("resolve library-prompt model %q: %w", m.config.LibraryPromptModel, err)
	}

	sys := "You help a user save the useful ask from a chat as a REUSABLE prompt in a shared prompt library, so they (or a teammate) can run it again later against a fresh agent with NO prior context. " +
		"From the transcript, produce: " +
		"(1) `content` — a single, clean, SELF-CONTAINED prompt that reproduces the useful work. It MUST stand alone: no references to \"this chat\", \"as we discussed\", \"the above\", or any prior context; restate the concrete task, its data sources, constraints, and the desired output format. Extract only the crux — ignore side questions, corrections that were superseded, and small talk; fold the user's refinements from later turns INTO the prompt (the final refined version of the ask, not its first draft). Keep it concrete as asked rather than inventing placeholders, and write it imperatively. " +
		"(2) `name` — a short (≤6 word) label for the library list. " +
		"(3) `description` — one sentence on what the prompt does, for the library list. " +
		"The user will review and edit the draft before saving, but write it ready-to-save as-is. " +
		structuredoutput.PromptAugmentation(json.RawMessage(libraryPromptSchema))

	var b strings.Builder
	b.WriteString("CONVERSATION TRANSCRIPT:\n")
	// Keep the MOST RECENT turns when truncating: the refined ask the user
	// wants to save is at the END of a long exploration, not the start.
	b.WriteString(keepRecentTranscript(transcript, libraryTranscriptMaxRunes))

	ag := fantasy.NewAgent(model,
		fantasy.WithSystemPrompt(sys),
		fantasy.WithTemperature(0.2),
		fantasy.WithMaxOutputTokens(libraryPromptMaxOutputTokens),
	)
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	maxTokens := int64(libraryPromptMaxOutputTokens)
	result, err := ag.Generate(ctx, fantasy.AgentCall{
		Messages:        []fantasy.Message{fantasy.NewUserMessage(b.String())},
		MaxOutputTokens: &maxTokens,
	})
	if err != nil {
		return nil, fmt.Errorf("library-prompt synthesis: %w", err)
	}

	var out strings.Builder
	for _, c := range result.Response.Content {
		if tc, ok := c.(fantasy.TextContent); ok {
			out.WriteString(tc.Text)
		}
	}
	validated, err := structuredoutput.ValidateOutput(out.String(), json.RawMessage(libraryPromptSchema))
	if err != nil {
		log.Printf("SuggestLibraryPrompt: output failed schema validation")
		return nil, fmt.Errorf("library-prompt synthesis produced no conforming output")
	}
	var d LibraryPromptDraft
	if err := json.Unmarshal(validated, &d); err != nil {
		return nil, fmt.Errorf("library-prompt synthesis: parse: %w", err)
	}
	d.Name = strings.TrimSpace(d.Name)
	d.Description = strings.TrimSpace(d.Description)
	d.Content = strings.TrimSpace(d.Content)
	if d.Content == "" {
		return nil, fmt.Errorf("library-prompt synthesis produced an empty prompt")
	}
	return &d, nil
}

const libraryPromptMaxOutputTokens = 1600

// libraryTranscriptMaxRunes bounds the transcript fed to the synthesizer.
const libraryTranscriptMaxRunes = 12000
