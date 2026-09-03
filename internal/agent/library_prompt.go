package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/structuredoutput"
)

// LibraryPromptInput is everything the synthesizer needs to turn a chat into
// a reusable workflow. The transcript carries the method (see the caller's
// workflowTranscriptFromHistory — tool calls included, not just text turns);
// the rest is the setup a fresh run would have to be told, because it is
// conversation configuration rather than anything said in the conversation.
type LibraryPromptInput struct {
	// Transcript is the rendered conversation: asks, answers, and the tools
	// the agent used between them.
	Transcript string
	// Title of the conversation, as a hint for naming.
	Title string
	// Persona the chat ran under, if any. A workflow that depends on one
	// should say so, since a teammate re-running it may not be using it.
	Persona string
	// Connectors names the optional MCP servers the conversation had enabled.
	// A saved workflow that silently needs one is a workflow that fails for
	// the next person.
	Connectors []string
}

// LibraryPromptDraft is a synthesized suggestion for saving a chat
// conversation as a reusable prompt-library entry. Content is a
// self-contained WORKFLOW TEMPLATE — objective, inputs, the steps and tools
// that did the work, and the expected output — Name a short label, and
// Description a one-line summary shown in the library list. The user reviews
// and edits the draft before saving; nothing is persisted by the synthesizer.
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
func (m *Manager) SuggestLibraryPrompt(ctx context.Context, in LibraryPromptInput) (*LibraryPromptDraft, error) {
	if strings.TrimSpace(in.Transcript) == "" {
		return nil, fmt.Errorf("empty transcript")
	}
	model, err := m.modelResolver().Resolve(ctx, m.config.LibraryPromptModel)
	if err != nil {
		return nil, fmt.Errorf("resolve library-prompt model %q: %w", m.config.LibraryPromptModel, err)
	}

	// The artifact is a WORKFLOW TEMPLATE, not a restatement of one ask. What
	// people want to keep off a good chat is the recipe — the sequence, the
	// tools, the gotchas — so a teammate can run the same procedure next
	// quarter against different inputs. An earlier version of this prompt
	// asked for "only the crux" of the ask and told the model to stay
	// concrete "rather than inventing placeholders", which produced exactly
	// the wrong thing: a single hardcoded question that reruns one analysis
	// on one client's data and teaches nobody the method.
	sys := "You turn a finished chat into a REUSABLE WORKFLOW TEMPLATE for a shared prompt library. Someone will paste it to a FRESH agent that has none of this conversation's context, months later, to run the same procedure against DIFFERENT inputs. " +
		"From the transcript — which includes the tools the agent actually used, marked [tool: name] — produce: " +
		"(1) `content` — the workflow template itself, in Markdown, structured as: " +
		"**Objective** (one or two sentences on what a run produces); " +
		"**Inputs** (what the person must supply, each as a `[BRACKETED PLACEHOLDER]` they fill in); " +
		"**Steps** (the numbered procedure that was actually followed, in order, naming the specific tools used at each step and what each step establishes — this is the heart of the template, so be concrete about method); " +
		"**Output** (the deliverable's format and structure, including how the final answer was presented); " +
		"**Notes** (constraints, quality bars, and pitfalls — especially anything that went wrong mid-run and had to be corrected, and any connector or persona the workflow depends on). " +
		"GENERALIZE the specifics: this run's client names, dates, filenames, figures and targets become placeholders, while the METHOD stays concrete and specific. A step that says \"analyze the data\" is worthless; say which tool, over what, producing what. " +
		"Carry forward the corrections the user made mid-chat as instructions, so the next run starts where this one ended up rather than repeating its mistakes. Ignore small talk and abandoned side quests. " +
		"(2) `name` — a short (≤6 word) label for the library list, naming the workflow rather than this run's subject. " +
		"(3) `description` — one sentence on what running it produces. " +
		"The user reviews and edits the draft before saving, but write it ready to save as-is. " +
		structuredoutput.PromptAugmentation(json.RawMessage(libraryPromptSchema))

	var b strings.Builder
	// Conversation setup the transcript cannot show. A workflow that needs a
	// connector, or that only reads right under a particular persona, has to
	// say so in its Notes — the next person re-running it may have neither.
	b.WriteString("CONVERSATION SETUP:\n")
	if t := strings.TrimSpace(in.Title); t != "" {
		fmt.Fprintf(&b, "- Title: %s\n", truncate(t, 200))
	}
	if p := strings.TrimSpace(in.Persona); p != "" {
		fmt.Fprintf(&b, "- Persona: %s\n", truncate(p, 100))
	}
	if names := trimmedNames(in.Connectors); len(names) > 0 {
		fmt.Fprintf(&b, "- Connectors enabled: %s\n", strings.Join(names, ", "))
	}
	b.WriteString("\nCONVERSATION TRANSCRIPT:\n")
	// Keep BOTH ENDS when truncating. keepRecentTranscript (the recurring-task
	// path) keeps only the tail, which is right when you want the final
	// refined ask — but a workflow's opening turns are where the objective and
	// the inputs are stated, and losing them costs the template its first two
	// sections. The middle of a long run is the most repetitive part and the
	// cheapest to drop.
	b.WriteString(keepTranscriptEnds(in.Transcript, libraryTranscriptMaxRunes))

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
	// Meter visibly (#1118), like the recurring-task synthesizer next door:
	// this is a conversation-level user action with no run session, so the
	// structured host log line is the only record that it cost anything.
	// Logged before validation — a non-conforming draft still cost money.
	logAuxUsage(agentcore.NewAuxUsageRecord(agentcore.AuxUsageLibraryPromptSynthesis, model.Model(), result))

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

// A workflow template carries five sections and a numbered procedure, so it
// needs more room than the single restated ask this used to emit.
const libraryPromptMaxOutputTokens = 3000

// libraryTranscriptMaxRunes bounds the transcript fed to the synthesizer. It
// is larger than the recurring-task budget because this transcript carries the
// tool sequence as well as the text turns — that sequence IS the workflow.
const libraryTranscriptMaxRunes = 24000

// keepTranscriptEnds trims the MIDDLE out of an over-long transcript, keeping
// the opening (objective, inputs, initial framing) and the close (the refined
// method and the delivered result). A third of the budget goes to the head and
// the rest to the tail: the tail is where corrections and the final shape
// live, but a workflow whose first turns are missing loses what it is FOR.
func keepTranscriptEnds(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	head := maxRunes / 3
	tail := maxRunes - head
	return string(r[:head]) + "\n\n…[middle of the conversation omitted]…\n\n" + string(r[len(r)-tail:])
}

// trimmedNames drops blanks and bounds each entry, so a malformed catalog
// cannot pad the prompt.
func trimmedNames(in []string) []string {
	out := make([]string, 0, len(in))
	for _, n := range in {
		if s := strings.TrimSpace(n); s != "" {
			out = append(out, truncate(s, 80))
		}
	}
	return out
}
