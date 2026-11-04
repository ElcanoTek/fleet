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

// PatrolDraftInput is what the Patrol Launchpad (docs/PATROL-LAUNCHPAD.md)
// knows when a person asks the assistant to write their patrol: the things
// they picked to watch, the connectors they switched on for it, the
// non-negotiables they saved as memories, how often it runs and how much it
// may do on its own. Every field is the person's own input from the wizard;
// nothing here comes from a conversation transcript.
type PatrolDraftInput struct {
	// Goals are the checks the person picked or typed ("Flag misconfigured
	// deals", "Alert when spend drops off", …).
	Goals []string
	// Notes is the free-text "anything else?" box.
	Notes string
	// Connectors are the connectors the patrol chat will have switched on,
	// with the one-line description the catalog gives each. A goal no
	// connector serves is the draft's to call out, not to paper over.
	Connectors []PatrolDraftConnector
	// Memories are the non-negotiables the person keeps for every chat. They
	// are already injected into every turn as memories; the draft is told
	// about them so it does not contradict one.
	Memories []string
	// Cadence is the schedule in words ("Weekdays at 08:00 (America/New_York)").
	Cadence string
	// Mode is how far the patrol may go on its own: "report", "suggest" or
	// "act". Anything else reads as "suggest".
	Mode string
	// Persona the patrol chat runs as, if not the workspace default.
	Persona string
}

// PatrolDraftConnector names one connector the patrol may use.
type PatrolDraftConnector struct {
	Name        string
	Description string
}

// PatrolDraft is the synthesized patrol: a short title for the sidebar and
// the Patrol bar, the standing instructions the assistant follows every time
// the patrol wakes, and a one-line summary the wizard shows above them. The
// person reviews and edits the draft before anything is created.
type PatrolDraft struct {
	Title   string `json:"title"`
	Prompt  string `json:"prompt"`
	Summary string `json:"summary"`
}

// patrolDraftSchema constrains the synthesizer's output; same shape rules as
// libraryPromptSchema (object-only, every field required, nothing extra).
const patrolDraftSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["title", "prompt", "summary"],
  "properties": {
    "title": { "type": "string" },
    "prompt": { "type": "string" },
    "summary": { "type": "string" }
  }
}`

// Bounds on what the wizard can hand the synthesizer. The handler enforces
// the counts; these keep any one entry from padding the prompt.
const (
	patrolDraftMaxGoals      = 12
	patrolDraftMaxConnectors = 40
	// The same cap as a turn's memory injection (httpapi memoryContents), so
	// the draft sees every memory a patrol run will be given.
	patrolDraftMaxMemories     = 50
	patrolDraftMaxOutputTokens = 2000
)

// SuggestPatrolDraft writes the standing instructions for a new patrol from
// the Launchpad's answers. It is the same short-lived, structured-output,
// host-side model call as SuggestRecurringTask (whose model it uses — a patrol
// IS a recurring task, run in a chat) with a hard timeout, and it returns an
// error on failure so the wizard can say so and keep its own template draft.
func (m *Manager) SuggestPatrolDraft(ctx context.Context, in PatrolDraftInput) (*PatrolDraft, error) {
	if len(trimmedNames(in.Goals)) == 0 && strings.TrimSpace(in.Notes) == "" {
		return nil, fmt.Errorf("nothing to patrol")
	}
	model, err := m.modelResolver().Resolve(ctx, m.config.RecurringTaskModel)
	if err != nil {
		return nil, fmt.Errorf("resolve patrol-draft model %q: %w", m.config.RecurringTaskModel, err)
	}

	sys := patrolDraftSystemPrompt + structuredoutput.PromptAugmentation(json.RawMessage(patrolDraftSchema))

	ag := fantasy.NewAgent(model,
		fantasy.WithSystemPrompt(sys),
		fantasy.WithTemperature(0.3),
		fantasy.WithMaxOutputTokens(patrolDraftMaxOutputTokens),
		fantasy.WithPrepareStep(agentcore.PromptCachingStep(model.Model(), agentcore.WithStablePrefixOnly())),
	)
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	maxTokens := int64(patrolDraftMaxOutputTokens)
	result, err := ag.Generate(ctx, fantasy.AgentCall{
		Messages:        []fantasy.Message{fantasy.NewUserMessage(patrolDraftUserMessage(in))},
		MaxOutputTokens: &maxTokens,
		ProviderOptions: agentcore.RoutingProviderOptions(model.Model()),
	})
	if err != nil {
		return nil, fmt.Errorf("patrol-draft synthesis: %w", err)
	}
	// Metered like the other pre-conversation synthesizers (#1118): logged
	// before validation, because a non-conforming draft still cost money.
	logAuxUsage(agentcore.NewAuxUsageRecord(agentcore.AuxUsagePatrolDraft, model.Model(), result))

	var out strings.Builder
	for _, c := range result.Response.Content {
		if tc, ok := c.(fantasy.TextContent); ok {
			out.WriteString(tc.Text)
		}
	}
	return parsePatrolDraft(out.String())
}

// parsePatrolDraft validates and normalizes the model's output.
func parsePatrolDraft(raw string) (*PatrolDraft, error) {
	validated, err := structuredoutput.ValidateOutput(raw, json.RawMessage(patrolDraftSchema))
	if err != nil {
		log.Printf("SuggestPatrolDraft: output failed schema validation")
		return nil, fmt.Errorf("patrol-draft synthesis produced no conforming output")
	}
	var d PatrolDraft
	if err := json.Unmarshal(validated, &d); err != nil {
		return nil, fmt.Errorf("patrol-draft synthesis: parse: %w", err)
	}
	d.Title = truncateAtRune(strings.TrimSpace(d.Title), 80)
	d.Summary = strings.TrimSpace(d.Summary)
	d.Prompt = strings.TrimSpace(d.Prompt)
	if d.Prompt == "" {
		return nil, fmt.Errorf("patrol-draft synthesis produced empty instructions")
	}
	if d.Title == "" {
		d.Title = "My patrol"
	}
	return &d, nil
}

// patrolDraftSystemPrompt is the stable prefix (cached). What it asks for is
// shaped by how a patrol actually runs: the same instructions are delivered
// to the assistant in the SAME conversation every time the schedule fires, so
// the assistant can see its earlier reports — hence "say what changed", not
// "repeat everything".
const patrolDraftSystemPrompt = "You write the standing instructions for a PATROL: a recurring check an AI assistant runs inside one chat conversation on a schedule. " +
	"Every time the schedule fires, your instructions are sent to the assistant as the next message in that same conversation, so it can see its previous reports. It can only use the connectors listed, and the person's memories are already given to it on every turn. " +
	"From the person's answers, produce: " +
	"(1) `prompt` — the instructions, in Markdown, written TO the assistant in the second person, structured as: " +
	"**Mission** (one or two sentences: what this patrol protects or improves for the person); " +
	"**Each run** (a numbered checklist, one item per goal, naming the specific connector to use and exactly what to look at; give a sensible default threshold where a goal needs one — e.g. \"spend down more than 25% versus the same hour yesterday\" — so the first run is useful, and say the person can change it); " +
	"**Report** (the format: start with one headline line — \"✅ All clear\" or \"⚠️ N items need you\" — then the items ordered by urgency, each with what was found, why it matters, and the recommended next step; compare with the previous run and say what is new, what is resolved and what is still open, rather than repeating unchanged findings in full; keep it scannable); " +
	"**Boundaries** (what the assistant may do on its own, following the autonomy mode given below, and that anything it cannot check because a connector is missing should be said once in the report, plainly, rather than guessed). " +
	"Autonomy modes: \"report\" means observe and report only, never change anything; \"suggest\" means recommend specific actions and stage them for the person's approval, but do not execute changes; \"act\" means it may take routine, reversible actions itself and must list every action it took in the report, while still asking first for anything costly, irreversible, or outside the goals. " +
	"Do not invent connectors, data, accounts or thresholds the person did not imply beyond a sensible default. Do not contradict a memory. If a goal has no connector that can serve it, keep the goal and tell the assistant to report what it needs. " +
	"(2) `title` — a short (≤5 word) name for the patrol, e.g. \"Morning deal & inbox sweep\". " +
	"(3) `summary` — one friendly sentence the person reads before launching, describing what will happen each run. " +
	"Write it ready to use as-is; the person can edit it before launching. "

// patrolDraftUserMessage renders the wizard's answers. Each list is bounded so
// a malformed request cannot pad the prompt.
func patrolDraftUserMessage(in PatrolDraftInput) string {
	var b strings.Builder
	b.WriteString("THE PERSON'S ANSWERS\n\nWhat to watch:\n")
	for _, g := range capList(trimmedLong(in.Goals, 300), patrolDraftMaxGoals) {
		fmt.Fprintf(&b, "- %s\n", g)
	}
	if n := strings.TrimSpace(in.Notes); n != "" {
		fmt.Fprintf(&b, "\nAnything else they said:\n%s\n", truncate(n, 2000))
	}
	b.WriteString("\nConnectors switched on for this patrol:\n")
	conns := 0
	for _, c := range in.Connectors {
		name := strings.TrimSpace(c.Name)
		if name == "" {
			continue
		}
		if conns == patrolDraftMaxConnectors {
			break
		}
		conns++
		if d := strings.TrimSpace(c.Description); d != "" {
			fmt.Fprintf(&b, "- %s — %s\n", truncate(name, 80), truncate(d, 240))
		} else {
			fmt.Fprintf(&b, "- %s\n", truncate(name, 80))
		}
	}
	if conns == 0 {
		b.WriteString("- (none — the assistant can only use its built-in tools)\n")
	}
	// The caller shortens each memory's content before appending its
	// kind/validity annotation (httpapi.memoryContentsCapped, 400 runes); this
	// looser bound only guards a caller that did not, and must not cut that
	// annotation off a memory that was shortened.
	if mems := capList(trimmedLong(in.Memories, 2000), patrolDraftMaxMemories); len(mems) > 0 {
		b.WriteString("\nTheir non-negotiables (already saved as memories):\n")
		for _, mem := range mems {
			fmt.Fprintf(&b, "- %s\n", mem)
		}
	}
	if c := strings.TrimSpace(in.Cadence); c != "" {
		fmt.Fprintf(&b, "\nSchedule: %s\n", truncate(c, 200))
	}
	fmt.Fprintf(&b, "Autonomy mode: %s\n", normalizePatrolMode(in.Mode))
	if p := strings.TrimSpace(in.Persona); p != "" {
		fmt.Fprintf(&b, "Runs as persona: %s\n", truncate(p, 100))
	}
	return b.String()
}

// normalizePatrolMode maps the wizard's autonomy choice onto the three modes
// the system prompt defines; anything unknown is the middle one.
func normalizePatrolMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "report":
		return "report"
	case "act":
		return "act"
	default:
		return "suggest"
	}
}

// trimmedLong is trimmedNames with a caller-chosen bound.
func trimmedLong(in []string, n int) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, truncate(s, n))
		}
	}
	return out
}

func capList(in []string, n int) []string {
	if len(in) > n {
		return in[:n]
	}
	return in
}
