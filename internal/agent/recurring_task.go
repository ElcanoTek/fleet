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

// RecurringTaskProposal is a synthesized suggestion for turning a chat
// conversation into a recurring scheduled task (#455). The Prompt is a clean,
// self-contained restatement of the useful work (no references to "this chat"),
// Cron a suggested 5-field cadence, Name a short label, and Rationale a
// one-line explanation shown to the user before they approve.
type RecurringTaskProposal struct {
	Name      string `json:"name"`
	Prompt    string `json:"prompt"`
	Cron      string `json:"cron"`
	Rationale string `json:"rationale"`
}

// recurringTaskSchema constrains the synthesizer's output. draft-07, object-only
// with additionalProperties:false so the model can't pad it; prompt + cron are
// required (a recurring task needs both).
const recurringTaskSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["name", "prompt", "cron", "rationale"],
  "properties": {
    "name": { "type": "string" },
    "prompt": { "type": "string" },
    "cron": { "type": "string" },
    "rationale": { "type": "string" }
  }
}`

// SuggestRecurringTask turns a conversation transcript into a proposed recurring
// scheduled task (#455): a clean standalone prompt, a suggested cron cadence, a
// short name, and a one-line rationale. It mirrors SuggestTitle/AnalyzeTaskFailure
// — a short-lived fantasy.NewAgent call through the host-side resolver against
// config.RecurringTaskModel, with structured-output validation and a hard
// timeout. Unlike the fire-and-forget helpers it returns an error on failure,
// because it backs a user-initiated action that should report "couldn't
// synthesize" rather than silently do nothing. `existingNames` are the user's
// current task names so the model avoids proposing a colliding one.
func (m *Manager) SuggestRecurringTask(ctx context.Context, transcript string, existingNames []string) (*RecurringTaskProposal, error) {
	if strings.TrimSpace(transcript) == "" {
		return nil, fmt.Errorf("empty transcript")
	}
	model, err := m.resolver.Resolve(ctx, m.config.RecurringTaskModel)
	if err != nil {
		return nil, fmt.Errorf("resolve recurring-task model %q: %w", m.config.RecurringTaskModel, err)
	}

	sys := "You help a user turn a one-time chat into a RECURRING scheduled task that an autonomous agent runs on a cadence with NO human present. " +
		"From the transcript, produce: " +
		"(1) `prompt` — a single, clean, SELF-CONTAINED instruction that reproduces the useful work each run. It MUST stand alone: no references to \"this chat\", \"as we discussed\", \"the above\", or any prior context; restate the concrete task, its data sources, and the desired output/delivery. Write it imperatively and specifically, as if it were the only thing the agent is told. " +
		"(2) `cron` — a STANDARD 5-field cron expression for a sensible cadence implied by the task (e.g. a daily report → \"0 9 * * *\"; a weekday check → \"0 8 * * MON-FRI\"). If the conversation gives no hint, choose a reasonable daily time. " +
		"(3) `name` — a short (≤6 word) label, distinct from the user's existing task names. " +
		"(4) `rationale` — one sentence on why this cadence + prompt fit. " +
		"The user will review and can edit or cancel before anything is created. " +
		structuredoutput.PromptAugmentation(json.RawMessage(recurringTaskSchema))

	var b strings.Builder
	if len(existingNames) > 0 {
		b.WriteString("EXISTING TASK NAMES (pick a distinct name):\n")
		for _, n := range existingNames {
			if s := strings.TrimSpace(n); s != "" {
				b.WriteString("- ")
				b.WriteString(truncate(s, 120))
				b.WriteString("\n")
			}
		}
		b.WriteString("\n")
	}
	b.WriteString("CONVERSATION TRANSCRIPT:\n")
	b.WriteString(truncate(transcript, 12000))

	ag := fantasy.NewAgent(model,
		fantasy.WithSystemPrompt(sys),
		fantasy.WithTemperature(0.2),
		fantasy.WithMaxOutputTokens(recurringTaskMaxOutputTokens),
	)
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	maxTokens := int64(recurringTaskMaxOutputTokens)
	result, err := ag.Generate(ctx, fantasy.AgentCall{
		Messages:        []fantasy.Message{fantasy.NewUserMessage(b.String())},
		MaxOutputTokens: &maxTokens,
	})
	if err != nil {
		return nil, fmt.Errorf("recurring-task synthesis: %w", err)
	}

	var out strings.Builder
	for _, c := range result.Response.Content {
		if tc, ok := c.(fantasy.TextContent); ok {
			out.WriteString(tc.Text)
		}
	}
	validated, err := structuredoutput.ValidateOutput(out.String(), json.RawMessage(recurringTaskSchema))
	if err != nil {
		log.Printf("SuggestRecurringTask: output failed schema validation")
		return nil, fmt.Errorf("recurring-task synthesis produced no conforming output")
	}
	var p RecurringTaskProposal
	if err := json.Unmarshal(validated, &p); err != nil {
		return nil, fmt.Errorf("recurring-task synthesis: parse: %w", err)
	}
	p.Name = strings.TrimSpace(p.Name)
	p.Prompt = strings.TrimSpace(p.Prompt)
	p.Cron = strings.TrimSpace(p.Cron)
	p.Rationale = strings.TrimSpace(p.Rationale)
	if p.Prompt == "" {
		return nil, fmt.Errorf("recurring-task synthesis produced an empty prompt")
	}
	return &p, nil
}

const recurringTaskMaxOutputTokens = 1200
