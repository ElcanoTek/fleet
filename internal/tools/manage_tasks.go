package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/truncate"
)

// ManageTasksToolName is the canonical name of the interactive manage_tasks tool.
const ManageTasksToolName = "manage_tasks"

// MaxManagedTasks caps how many tasks one approved call may touch. A bulk edit
// is a single click for the human and N writes for the system, so the ceiling
// exists to keep a mistyped filter from being an estate-wide event. Above it the
// call is refused with the match count, so the user narrows the filter rather
// than discovering the blast radius afterwards.
const MaxManagedTasks = 25

// MaxStopTaskIDs caps an explicit stop list. Deliberately much smaller than
// MaxManagedTasks: stopping is not reversible from chat.
const MaxStopTaskIDs = 10

// MaxPromptEdits caps how many exact-text edits one update may carry. Every
// edit is rendered on the approval card, and a card too long to read is not a
// card anybody reviewed.
const MaxPromptEdits = 20

// MaxShownPromptRunes bounds each prompt action="show" hands the model. A task
// prompt is normally a few kilobytes; the bound only keeps one pathological
// prompt from flooding the turn. A cut prompt is flagged, so the model knows it
// has not seen the whole text and must not rebuild it from what it saw.
const MaxShownPromptRunes = 20000

// TaskMatch selects tasks by property instead of by id. It exists because the
// actual request was "update ALL the update dashboard jobs" — naming twelve
// UUIDs by hand is not a workflow anyone completes.
//
// Every field ANDs. An empty match is refused rather than treated as "all".
type TaskMatch struct {
	Query string `json:"query,omitempty" description:"Substring matched against task name and prompt, e.g. 'update dashboard'."`
	Tag   string `json:"tag,omitempty" description:"Only tasks carrying this tag."`
	Model string `json:"model,omitempty" description:"Only tasks currently pinned to this model slug. Useful for migrating a whole fleet off a retired model."`
}

// IsEmpty reports whether the match names no criterion at all.
func (m *TaskMatch) IsEmpty() bool {
	if m == nil {
		return true
	}
	return strings.TrimSpace(m.Query) == "" &&
		strings.TrimSpace(m.Tag) == "" &&
		strings.TrimSpace(m.Model) == ""
}

// PromptEdit is one exact-text replacement inside a task's STORED prompt.
//
// It exists because the only way to change a prompt used to be a full
// replacement, and the model had no way to read the prompt it was replacing: a
// user asked for a small addition to a scheduled email, the model retyped the
// prompt from what it remembered of the conversation, and the rules the
// working prompt had accumulated were silently dropped. An edit is applied
// server-side to the text that is actually stored when the user approves, so
// nothing the edit does not name can be lost.
type PromptEdit struct {
	Old string `json:"old" description:"Text copied EXACTLY from the task's current prompt (read it first with action='show'). It must occur exactly once in the prompt; include a few surrounding words if it is short or repeated."`
	New string `json:"new" description:"Replacement text for old. Empty deletes old."`
}

// ManageTasksParams is the agent-facing input schema for manage_tasks (#1152).
//
// One tool rather than update_task + delete_task: the interactive roster runs
// near the provider tool ceiling once per-user MCP servers load, the two share
// their whole selector and approval surface, and the approval card — not the
// tool name — is what actually prevents a wrong destructive call. The read-only
// "show" action shares the selector for the same reason, and because the model
// must read exactly the tasks the update would touch.
type ManageTasksParams struct {
	Action        string       `json:"action" description:"'show' to read the current prompt and settings of existing tasks (read-only, runs at once with no approval card), 'update' to change them, or 'stop' to cancel them (a recurring job stops recurring). Required."`
	TaskIDs       []string     `json:"task_ids,omitempty" description:"Explicit task ids to act on. REQUIRED for action='stop'. For 'show' and 'update', supply either this or match."`
	Match         *TaskMatch   `json:"match,omitempty" description:"Select tasks by property instead of by id. Valid for 'show' and 'update' — stopping always requires explicit ids, because it cannot be undone from chat."`
	Prompt        string       `json:"prompt,omitempty" description:"FULL replacement prompt (update only): anything in a task's current prompt that is not in this text is deleted. Only for a deliberate rewrite the user asked for; to adjust a prompt use prompt_edits or prompt_append instead. Omit to leave each task's prompt alone."`
	PromptEdits   []PromptEdit `json:"prompt_edits,omitempty" description:"Exact-text edits applied in order to each task's CURRENT stored prompt when the user approves (update only). Everything the edits do not name is kept word for word. A task whose prompt does not contain an edit's old text exactly once is skipped, unchanged, with the reason. Cannot be combined with prompt."`
	PromptAppend  string       `json:"prompt_append,omitempty" description:"Text added to the end of each task's CURRENT stored prompt, after a blank line, when the user approves (update only; applied after prompt_edits). Cannot be combined with prompt."`
	Cron          string       `json:"cron,omitempty" description:"Replacement 5-field cron expression (update only), e.g. '0 9 * * MON-FRI'. Translate the user's words to cron yourself. Omit to leave the schedule alone."`
	Model         string       `json:"model,omitempty" description:"Replacement model slug (update only). Omit to leave it alone."`
	MaxIterations int          `json:"max_iterations,omitempty" description:"Replacement per-run step cap (update only). Omit to leave it alone."`
	AddTags       []string     `json:"add_tags,omitempty" description:"Tags to add (update only; lowercase alphanumeric, '-', and '.', e.g. 'pages', 'daily-refresh')."`
	RemoveTags    []string     `json:"remove_tags,omitempty" description:"Tags to remove (update only; lowercase alphanumeric, '-', and '.')."`
}

const manageTasksDescription = `Reads, changes or stops EXISTING scheduled tasks in the Fleet orchestrator (the Background tasks page, formerly the Operations Center), so a user can maintain their jobs from chat instead of opening each one by hand.

action="show" — read-only: returns the current prompt, schedule, model and tags of the tasks you select by id OR by match. Runs immediately, with no approval card. ALWAYS show a task before changing its prompt: you cannot see a stored prompt any other way, and your memory of the conversation is not the prompt.

action="update" — change the prompt, schedule, model, step cap, or tags on tasks you select by id OR by match (e.g. every task tagged "pages", every task still pinned to a retired model). This is the bulk path: "change all the dashboard update jobs to run at 7am" is one call.
  Prompt changes — pick the narrowest:
  • prompt_edits: [{old, new}] — exact-text replacements applied to each task's CURRENT stored prompt at approval. Copy old verbatim from action="show". Use this for any adjustment ("also include the last 14 days", "change the subject line"); every other word of the prompt is kept.
  • prompt_append — add a section at the end, keeping the rest.
  • prompt — a FULL replacement: everything not in your text is deleted. Use it only when the user asked for a rewrite, never for a "slight adjustment".

action="stop" — cancel tasks. A recurring job stops recurring; a running one halts at its next checkpoint. This ALWAYS requires explicit task_ids: stopping cannot be undone from chat, so it never runs against a filter the user has not seen resolved.

Use schedule_task to CREATE a task; this tool only touches ones that already exist.

"update" and "stop" are CRITICAL actions: the user sees an approval card naming exactly what will change (each prompt edit as old → new) and must click Approve before anything is written. After approval the tool reports every task it touched, by name, and any it skipped with the reason. If you do not know a task's id, describe it with match rather than guessing — a wrong id silently edits somebody else's job.`

// ManagedTaskView is one task as action="show" returns it to the model.
type ManagedTaskView struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Status          string   `json:"status"`
	Cron            string   `json:"cron,omitempty"`
	Timezone        string   `json:"timezone,omitempty"`
	Model           string   `json:"model,omitempty"`
	Tags            []string `json:"tags,omitempty"`
	Prompt          string   `json:"prompt"`
	PromptTruncated bool     `json:"prompt_truncated,omitempty"`
}

// ManagedTaskSkip names a selected task whose prompt is not shown, and why.
type ManagedTaskSkip struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// ManageTasksShowResult is what action="show" returns to the model.
type ManageTasksShowResult struct {
	Tasks    []ManagedTaskView `json:"tasks"`
	NotShown []ManagedTaskSkip `json:"not_shown,omitempty"`
	Note     string            `json:"note"`
}

// ManageTasksShowHandler resolves a show call's selector against the
// orchestrator for the person at the keyboard. Injected onto the turn context
// by httpapi, which applies the same selection and ownership rules the
// approved update path applies — so show never reveals a prompt the same
// person could not change.
type ManageTasksShowHandler func(ctx context.Context, params ManageTasksParams) (*ManageTasksShowResult, error)

type manageTasksShowHandlerKey struct{}

// WithManageTasksShowHandler attaches a per-turn ManageTasksShowHandler to ctx.
func WithManageTasksShowHandler(ctx context.Context, h ManageTasksShowHandler) context.Context {
	if h == nil {
		return ctx
	}
	return context.WithValue(ctx, manageTasksShowHandlerKey{}, h)
}

func manageTasksShowHandlerFromContext(ctx context.Context) ManageTasksShowHandler {
	if ctx == nil {
		return nil
	}
	h, _ := ctx.Value(manageTasksShowHandlerKey{}).(ManageTasksShowHandler)
	return h
}

const manageTasksShowNote = "These are the tasks' current stored prompts. To adjust one, call manage_tasks action='update' with prompt_edits (each old copied exactly from this text) or prompt_append; a full prompt replaces everything shown here."

// IsManageTasksShow reports whether a raw manage_tasks argument string is a
// read-only action="show" call. The interactive approval gate uses it to let
// show run without a card. It is a pass-through decision only: the tool's Run
// executes nothing but a read for show, and stays a tripwire for every other
// action, so a misclassified call still cannot write.
func IsManageTasksShow(rawInput string) bool {
	var args struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal([]byte(rawInput), &args); err != nil {
		return false
	}
	return strings.TrimSpace(args.Action) == ManageTasksActionShow
}

// NewManageTasksTool returns the manage_tasks tool. For update and stop its Run
// is a deliberate error: the interactive orchestration layer intercepts those
// calls and the real work happens in the approval-resolution handler. Only the
// read-only show action runs here, through the per-turn show handler.
func NewManageTasksTool() fantasy.AgentTool {
	return fantasy.NewAgentTool(ManageTasksToolName, manageTasksDescription,
		func(ctx context.Context, params ManageTasksParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if strings.TrimSpace(params.Action) != ManageTasksActionShow {
				return fantasy.NewTextErrorResponse(
					"manage_tasks was executed directly — orchestration should have staged it for user approval. This is a bug.",
				), fmt.Errorf("manage_tasks bypass")
			}
			if err := params.Validate(); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			h := manageTasksShowHandlerFromContext(ctx)
			if h == nil {
				return fantasy.NewTextErrorResponse(
					"manage_tasks action='show' is not available here. Ask the user to open the task on the Background tasks page and paste its prompt.",
				), nil
			}
			res, err := h(ctx, params)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if res == nil {
				res = &ManageTasksShowResult{}
			}
			if res.Tasks == nil {
				res.Tasks = []ManagedTaskView{}
			}
			for i := range res.Tasks {
				if utf8.RuneCountInString(res.Tasks[i].Prompt) > MaxShownPromptRunes {
					res.Tasks[i].Prompt = truncate.Runes(res.Tasks[i].Prompt, MaxShownPromptRunes, "")
					res.Tasks[i].PromptTruncated = true
				}
			}
			res.Note = manageTasksShowNote
			if out, mErr := json.MarshalIndent(res, "", "  "); mErr == nil {
				return fantasy.NewTextResponse(string(out)), nil
			}
			return fantasy.NewTextErrorResponse("manage_tasks: could not encode the tasks"), nil
		})
}

// Actions accepted by manage_tasks.
const (
	ManageTasksActionShow   = "show"
	ManageTasksActionUpdate = "update"
	ManageTasksActionStop   = "stop"
)

// Validate checks everything that needs no storage: a known action, a selector
// that names something, a change set that changes something, and the
// stop-requires-explicit-ids rule. Cron validity is left to the storage update
// path, the single source of truth, exactly as schedule_task leaves it; whether
// a prompt edit's old text occurs exactly once can only be checked against the
// stored prompt, at approval.
func (p ManageTasksParams) Validate() error {
	action := strings.TrimSpace(p.Action)
	ids := p.CleanTaskIDs()
	switch action {
	case ManageTasksActionShow:
		if len(ids) == 0 && p.Match.IsEmpty() {
			return fmt.Errorf("manage_tasks: supply task_ids or a non-empty match naming which tasks to show")
		}
		if len(ids) > 0 && !p.Match.IsEmpty() {
			return fmt.Errorf("manage_tasks: supply EITHER task_ids OR match, not both")
		}
		if p.HasChanges() {
			return fmt.Errorf("manage_tasks: action 'show' only reads; drop the change fields, or use action 'update' to change the tasks")
		}
	case ManageTasksActionUpdate:
		if len(ids) == 0 && p.Match.IsEmpty() {
			return fmt.Errorf("manage_tasks: supply task_ids or a non-empty match naming which tasks to update")
		}
		if len(ids) > 0 && !p.Match.IsEmpty() {
			return fmt.Errorf("manage_tasks: supply EITHER task_ids OR match, not both — a selector that means two things is a selector nobody can review")
		}
		if !p.HasChanges() {
			return fmt.Errorf("manage_tasks: an update must change something (prompt, prompt_edits, prompt_append, cron, model, max_iterations, add_tags, or remove_tags)")
		}
		if err := p.validatePromptChanges(); err != nil {
			return err
		}
	case ManageTasksActionStop:
		if len(ids) == 0 {
			return fmt.Errorf("manage_tasks: action 'stop' requires explicit task_ids — stopping cannot be undone from chat, so it never runs against a filter. Ask the user which jobs to stop, or update them instead")
		}
		if !p.Match.IsEmpty() {
			return fmt.Errorf("manage_tasks: action 'stop' does not accept match; list the task_ids explicitly")
		}
		if len(ids) > MaxStopTaskIDs {
			return fmt.Errorf("manage_tasks: action 'stop' accepts at most %d task_ids at a time (got %d)", MaxStopTaskIDs, len(ids))
		}
		if p.HasChanges() {
			return fmt.Errorf("manage_tasks: action 'stop' does not change fields; drop prompt/cron/model/max_iterations/tags or use action 'update'")
		}
	case "":
		return fmt.Errorf("manage_tasks: action is required ('show', 'update' or 'stop')")
	default:
		return fmt.Errorf("manage_tasks: unknown action %q (want 'show', 'update' or 'stop')", action)
	}
	if len(ids) > MaxManagedTasks {
		return fmt.Errorf("manage_tasks: at most %d task_ids per call (got %d)", MaxManagedTasks, len(ids))
	}
	return nil
}

// validatePromptChanges checks the prompt fields of an update against each
// other. A full replacement and an edit of the current prompt contradict each
// other, so asking for both is refused rather than resolved by a precedence
// rule the card would have to explain.
func (p ManageTasksParams) validatePromptChanges() error {
	if strings.TrimSpace(p.Prompt) != "" && (len(p.PromptEdits) > 0 || strings.TrimSpace(p.PromptAppend) != "") {
		return fmt.Errorf("manage_tasks: prompt replaces the whole prompt, while prompt_edits and prompt_append change the current one — use one or the other")
	}
	if len(p.PromptEdits) > MaxPromptEdits {
		return fmt.Errorf("manage_tasks: at most %d prompt_edits per call (got %d)", MaxPromptEdits, len(p.PromptEdits))
	}
	for i, e := range p.PromptEdits {
		if e.Old == "" {
			return fmt.Errorf("manage_tasks: prompt_edits[%d].old is empty; copy the exact text to replace from action='show' (use prompt_append to add text at the end)", i)
		}
		if e.Old == e.New {
			return fmt.Errorf("manage_tasks: prompt_edits[%d] replaces a text with itself", i)
		}
	}
	return nil
}

// HasChanges reports whether the params carry at least one field mutation.
func (p ManageTasksParams) HasChanges() bool {
	return strings.TrimSpace(p.Prompt) != "" ||
		len(p.PromptEdits) > 0 ||
		strings.TrimSpace(p.PromptAppend) != "" ||
		strings.TrimSpace(p.Cron) != "" ||
		strings.TrimSpace(p.Model) != "" ||
		p.MaxIterations > 0 ||
		len(cleanStrings(p.AddTags)) > 0 ||
		len(cleanStrings(p.RemoveTags)) > 0
}

// ApplyPromptEdits applies prompt_edits and then prompt_append to one task's
// stored prompt. Each edit's old text must occur exactly once in the text as the
// earlier edits left it; otherwise nothing is applied and the error says which
// edit failed, so the caller can skip that task unchanged. A miss is not a
// guess: replacing the first of several occurrences, or none, would change a
// prompt in a way the card did not show.
func ApplyPromptEdits(current string, edits []PromptEdit, appendText string) (string, error) {
	out := current
	for i, e := range edits {
		at := strings.Index(out, e.Old)
		if at < 0 {
			return "", fmt.Errorf("prompt edit %d: the text to replace (%q) is not in this task's prompt", i+1, truncate.Runes(truncate.OneLine(e.Old), 60, "…"))
		}
		// A second start position anywhere after the first one's first rune —
		// overlapping included ("ana" in "banana") — makes the edit ambiguous.
		// strings.Count counts only non-overlapping matches and would miss it.
		_, size := utf8.DecodeRuneInString(out[at:])
		if strings.Contains(out[at+size:], e.Old) {
			return "", fmt.Errorf("prompt edit %d: the text to replace (%q) appears more than once in this task's prompt; include more surrounding text so it matches exactly once", i+1, truncate.Runes(truncate.OneLine(e.Old), 60, "…"))
		}
		out = out[:at] + e.New + out[at+len(e.Old):]
	}
	if appendText = strings.TrimSpace(appendText); appendText != "" {
		switch {
		case strings.TrimSpace(out) == "":
			out = appendText
		case strings.HasSuffix(out, "\n\n"):
			out += appendText
		case strings.HasSuffix(out, "\n"):
			out += "\n" + appendText
		default:
			out += "\n\n" + appendText
		}
	}
	return out, nil
}

// CleanTaskIDs returns the trimmed, de-duplicated, non-empty ids in order.
func (p ManageTasksParams) CleanTaskIDs() []string {
	seen := make(map[string]bool, len(p.TaskIDs))
	out := make([]string, 0, len(p.TaskIDs))
	for _, id := range p.TaskIDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// ChangeSummary renders the mutation as short human-readable lines for the
// approval card. Empty for a stop. Prompt text itself is not squeezed into
// these lines: the card renders edits, appended text and a full replacement in
// their own blocks (approval_summary.go), and the line here says which kind of
// prompt change it is — the difference between "edits the prompt" and
// "replaces all of it" is the thing the approver most needs to see.
func (p ManageTasksParams) ChangeSummary() []string {
	var out []string
	if v := strings.TrimSpace(p.Cron); v != "" {
		out = append(out, "schedule → "+v)
	}
	if v := strings.TrimSpace(p.Model); v != "" {
		out = append(out, "model → "+v)
	}
	if p.MaxIterations > 0 {
		out = append(out, fmt.Sprintf("max_iterations → %d", p.MaxIterations))
	}
	if v := strings.TrimSpace(p.Prompt); v != "" {
		out = append(out, fmt.Sprintf("prompt → REPLACED IN FULL (%d characters): anything not in the new text is removed", utf8.RuneCountInString(v)))
	}
	if n := len(p.PromptEdits); n == 1 {
		out = append(out, "prompt → 1 exact-text edit; the rest of the prompt is kept")
	} else if n > 1 {
		out = append(out, fmt.Sprintf("prompt → %d exact-text edits; the rest of the prompt is kept", n))
	}
	if strings.TrimSpace(p.PromptAppend) != "" {
		out = append(out, "prompt → text added at the end; the rest of the prompt is kept")
	}
	if tags := cleanStrings(p.AddTags); len(tags) > 0 {
		out = append(out, "add tags → "+strings.Join(tags, ", "))
	}
	if tags := cleanStrings(p.RemoveTags); len(tags) > 0 {
		out = append(out, "remove tags → "+strings.Join(tags, ", "))
	}
	return out
}

// MatchSummary renders the selector for the approval card.
func (p ManageTasksParams) MatchSummary() []string {
	var out []string
	if p.Match == nil {
		return out
	}
	if v := strings.TrimSpace(p.Match.Query); v != "" {
		out = append(out, "name or prompt contains "+v)
	}
	if v := strings.TrimSpace(p.Match.Tag); v != "" {
		out = append(out, "tagged "+v)
	}
	if v := strings.TrimSpace(p.Match.Model); v != "" {
		out = append(out, "currently on model "+v)
	}
	return out
}

func cleanStrings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
