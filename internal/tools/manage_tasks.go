package tools

import (
	"context"
	"fmt"
	"strings"

	"charm.land/fantasy"
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

// ManageTasksParams is the agent-facing input schema for manage_tasks (#1152).
//
// One tool rather than update_task + delete_task: the interactive roster runs
// near the provider tool ceiling once per-user MCP servers load, the two share
// their whole selector and approval surface, and the approval card — not the
// tool name — is what actually prevents a wrong destructive call.
type ManageTasksParams struct {
	Action        string     `json:"action" description:"'update' to change existing scheduled tasks, or 'stop' to cancel them (a recurring job stops recurring). Required."`
	TaskIDs       []string   `json:"task_ids,omitempty" description:"Explicit task ids to act on. REQUIRED for action='stop'. For 'update', supply either this or match."`
	Match         *TaskMatch `json:"match,omitempty" description:"Select tasks by property instead of by id. Only valid for action='update' — stopping always requires explicit ids, because it cannot be undone from chat."`
	Prompt        string     `json:"prompt,omitempty" description:"Replacement prompt (update only). Omit to leave each task's prompt alone."`
	Cron          string     `json:"cron,omitempty" description:"Replacement 5-field cron expression (update only), e.g. '0 9 * * MON-FRI'. Translate the user's words to cron yourself. Omit to leave the schedule alone."`
	Model         string     `json:"model,omitempty" description:"Replacement model slug (update only). Omit to leave it alone."`
	MaxIterations int        `json:"max_iterations,omitempty" description:"Replacement per-run step cap (update only). Omit to leave it alone."`
	AddTags       []string   `json:"add_tags,omitempty" description:"Tags to add (update only)."`
	RemoveTags    []string   `json:"remove_tags,omitempty" description:"Tags to remove (update only)."`
}

const manageTasksDescription = `Changes or stops EXISTING scheduled tasks in the Fleet orchestrator (the Operations Center), so a user can maintain their jobs from chat instead of opening each one by hand.

action="update" — change the prompt, schedule, model, step cap, or tags on tasks you select by id OR by match (e.g. every task tagged "pages", every task still pinned to a retired model). This is the bulk path: "change all the dashboard update jobs to run at 7am" is one call.

action="stop" — cancel tasks. A recurring job stops recurring; a running one halts at its next checkpoint. This ALWAYS requires explicit task_ids: stopping cannot be undone from chat, so it never runs against a filter the user has not seen resolved.

Use schedule_task to CREATE a task; this tool only touches ones that already exist.

This is a CRITICAL action: the user sees an approval card naming exactly what will change and must click Approve before anything is written. After approval the tool reports every task it touched, by name. If you do not know a task's id, describe it with match rather than guessing — a wrong id silently edits somebody else's job.`

// NewManageTasksTool returns the manage_tasks tool. Like schedule_task its Run
// is a deliberate error: the interactive orchestration layer intercepts every
// call and the real work happens in the approval-resolution handler.
func NewManageTasksTool() fantasy.AgentTool {
	return fantasy.NewAgentTool(ManageTasksToolName, manageTasksDescription,
		func(_ context.Context, _ ManageTasksParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextErrorResponse(
				"manage_tasks was executed directly — orchestration should have staged it for user approval. This is a bug.",
			), fmt.Errorf("manage_tasks bypass")
		})
}

// Actions accepted by manage_tasks.
const (
	ManageTasksActionUpdate = "update"
	ManageTasksActionStop   = "stop"
)

// Validate checks everything that needs no storage: a known action, a selector
// that names something, a change set that changes something, and the
// stop-requires-explicit-ids rule. Cron validity is left to the storage update
// path, the single source of truth, exactly as schedule_task leaves it.
func (p ManageTasksParams) Validate() error {
	action := strings.TrimSpace(p.Action)
	ids := p.CleanTaskIDs()
	switch action {
	case ManageTasksActionUpdate:
		if len(ids) == 0 && p.Match.IsEmpty() {
			return fmt.Errorf("manage_tasks: supply task_ids or a non-empty match naming which tasks to update")
		}
		if len(ids) > 0 && !p.Match.IsEmpty() {
			return fmt.Errorf("manage_tasks: supply EITHER task_ids OR match, not both — a selector that means two things is a selector nobody can review")
		}
		if !p.HasChanges() {
			return fmt.Errorf("manage_tasks: an update must change something (prompt, cron, model, max_iterations, add_tags, or remove_tags)")
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
		return fmt.Errorf("manage_tasks: action is required ('update' or 'stop')")
	default:
		return fmt.Errorf("manage_tasks: unknown action %q (want 'update' or 'stop')", action)
	}
	if len(ids) > MaxManagedTasks {
		return fmt.Errorf("manage_tasks: at most %d task_ids per call (got %d)", MaxManagedTasks, len(ids))
	}
	return nil
}

// HasChanges reports whether the params carry at least one field mutation.
func (p ManageTasksParams) HasChanges() bool {
	return strings.TrimSpace(p.Prompt) != "" ||
		strings.TrimSpace(p.Cron) != "" ||
		strings.TrimSpace(p.Model) != "" ||
		p.MaxIterations > 0 ||
		len(cleanStrings(p.AddTags)) > 0 ||
		len(cleanStrings(p.RemoveTags)) > 0
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
// approval card. Empty for a stop.
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
		out = append(out, "prompt → replaced ("+truncateForCard(v, 80)+")")
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

// truncateForCard bounds by RUNES so a multibyte prompt is never cut mid-rune
// into a replacement character on the card.
func truncateForCard(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}
