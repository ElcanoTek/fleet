package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/httpapi"
	"github.com/ElcanoTek/fleet/internal/sched/budget"
	scheddb "github.com/ElcanoTek/fleet/internal/sched/db"
	schedmodels "github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// taskManagerProvider adapts the sched storage layer to the chat manage_tasks
// seam (#1152): show, update or stop tasks that already exist.
//
// Chat gains no authority the same person lacks through the API. Every target
// is resolved, then filtered to tasks the APPROVING user created — the same
// ownership rule handlers.CancelTask applies to a non-admin stop — and anything
// dropped is reported back by name with the reason rather than silently
// omitted. A count without names is a bulk edit nobody can check. A read-only
// show goes through the same resolution and the same ownership filter, so the
// model reads the prompts of exactly the tasks the person could change.
func taskManagerProvider(schedStorage *storage.Storage) func(context.Context, httpapi.TaskMutationRequest) (*httpapi.TaskMutationResult, error) {
	return func(ctx context.Context, req httpapi.TaskMutationRequest) (*httpapi.TaskMutationResult, error) {
		// The approving person's one identity row gives the owner UUID (#2049
		// M4): every enabled account has one, Operations Center access or not.
		// Only tasks that UUID created are touched (below), so this grants a
		// chat-only account nothing over anyone else's tasks.
		owner := accountOwnerID(ctx, schedStorage, req.RequestedBy)
		if owner == nil {
			return nil, fmt.Errorf("cannot identify %q as a Fleet account; edit the task on the Background tasks page instead", req.RequestedBy)
		}

		candidates, err := resolveManagedTasks(schedStorage, req, owner)
		if err != nil {
			return nil, err
		}
		res := &httpapi.TaskMutationResult{Matched: len(candidates)}
		for _, t := range candidates {
			label := taskMutationLabel(t)
			// Ownership is re-checked per task, not just baked into the query,
			// so an explicit task_ids list cannot reach somebody else's job.
			if t.CreatedBy == nil || *t.CreatedBy != *owner {
				res.Skipped = append(res.Skipped, httpapi.TaskMutationEntry{
					ID: t.ID.String(), Label: label,
					Detail: "you did not create this task; an admin can change it on the Background tasks page",
				})
				continue
			}
			if req.Action == tools.ManageTasksActionShow {
				res.Shown = append(res.Shown, taskSnapshot(t, label))
				continue
			}
			if req.Action == tools.ManageTasksActionStop {
				if t.Status.IsTerminal() {
					res.Skipped = append(res.Skipped, httpapi.TaskMutationEntry{
						ID: t.ID.String(), Label: label, Detail: "already " + string(t.Status) + "; nothing to stop",
					})
					continue
				}
				if _, err := schedStorage.CancelTaskAtomic(t.ID, "stopped from chat by "+req.RequestedBy); err != nil {
					res.Skipped = append(res.Skipped, httpapi.TaskMutationEntry{ID: t.ID.String(), Label: label, Detail: err.Error()})
					continue
				}
				res.Changed = append(res.Changed, httpapi.TaskMutationEntry{ID: t.ID.String(), Label: label, Detail: "stopped"})
				continue
			}
			detail, err := applyTaskMutation(ctx, schedStorage, t, req)
			if err != nil {
				res.Skipped = append(res.Skipped, httpapi.TaskMutationEntry{ID: t.ID.String(), Label: label, Detail: err.Error()})
				continue
			}
			res.Changed = append(res.Changed, httpapi.TaskMutationEntry{ID: t.ID.String(), Label: label, Detail: detail})
		}
		return res, nil
	}
}

// resolveManagedTasks turns a mutation request's selector into concrete tasks.
// An explicit id list is fetched one by one so a bad id is reported as a bad id;
// a match runs through the same filter the task list uses, bounded so a mistyped
// filter is refused with its own match count rather than executed.
//
// No context parameter: both reads it makes are the non-context storage
// variants, and taking a ctx it cannot honor would advertise a cancellation
// story that is not true.
func resolveManagedTasks(schedStorage *storage.Storage, req httpapi.TaskMutationRequest, owner *uuid.UUID) ([]*schedmodels.Task, error) {
	if len(req.TaskIDs) > 0 {
		out := make([]*schedmodels.Task, 0, len(req.TaskIDs))
		for _, raw := range req.TaskIDs {
			id, err := uuid.Parse(strings.TrimSpace(raw))
			if err != nil {
				return nil, fmt.Errorf("%q is not a task id", raw)
			}
			t, err := schedStorage.GetTask(id)
			if err != nil || t == nil {
				return nil, fmt.Errorf("task %s not found", raw)
			}
			out = append(out, t)
		}
		return out, nil
	}
	if req.Match == nil {
		return nil, fmt.Errorf("no tasks selected")
	}
	filter := scheddb.TaskFilter{VisibleToUserID: owner}
	if q := strings.TrimSpace(req.Match.Query); q != "" {
		filter.Query = &q
	}
	if tag := strings.TrimSpace(req.Match.Tag); tag != "" {
		filter.Tags = []string{tag}
	}
	model := strings.TrimSpace(req.Match.Model)
	if model == "" {
		// One over the cap, so "you matched too many" is honest about there
		// being more rather than reporting exactly the ceiling.
		tasks, total, err := schedStorage.GetTasksFiltered(filter, tools.MaxManagedTasks+1, 0)
		if err != nil {
			return nil, err
		}
		if len(tasks) > tools.MaxManagedTasks {
			return nil, fmt.Errorf("that selector matches %d tasks, above the %d-task limit for one approved change; narrow it (add a tag, or name the tasks explicitly)", total, tools.MaxManagedTasks)
		}
		return tasks, nil
	}
	// The model predicate is applied here, not in SQL, so it must see every
	// row the storage filter matches: filtering only the first page would
	// silently drop later matches (and report the short list as complete).
	// Page through the requester's own tasks until the matches pass the cap.
	var tasks []*schedmodels.Task
	for offset := 0; ; {
		page, total, err := schedStorage.GetTasksFiltered(filter, managedTasksScanPage, offset)
		if err != nil {
			return nil, err
		}
		for _, t := range page {
			if t.Model != nil && strings.EqualFold(strings.TrimSpace(*t.Model), model) {
				tasks = append(tasks, t)
			}
		}
		offset += len(page)
		if len(page) == 0 || offset >= total || len(tasks) > tools.MaxManagedTasks {
			break
		}
	}
	if len(tasks) > tools.MaxManagedTasks {
		return nil, fmt.Errorf("that selector matches more than %d tasks, the limit for one approved change; narrow it (add a tag, or name the tasks explicitly)", tools.MaxManagedTasks)
	}
	return tasks, nil
}

// managedTasksScanPage is the page size resolveManagedTasks reads with while
// applying the in-memory model predicate.
const managedTasksScanPage = 200

// applyTaskMutation writes one task's field changes and returns a short
// description of what actually changed, for the chat-visible report.
//
// The definition changes are overlaid on storage.TaskEditFromTask and written
// through UpdateEditableTask — the same row-locked, still-editable-checked,
// dispatch-state-recomputing path PUT /tasks/{id} uses — never through the
// unlocked full-row UpdateTask upsert. That upsert wrote the status, lease and
// result columns of the read above back over whatever the scheduler and runner
// had done since (a running task rewound to pending and executed twice; a
// finished one flipped back to non-terminal, its result erased — the #1104
// shape). A task that was claimed or finished in the meantime is now refused
// with a plain reason instead.
func applyTaskMutation(ctx context.Context, schedStorage *storage.Storage, t *schedmodels.Task, req httpapi.TaskMutationRequest) (string, error) {
	var changed []string
	edit := storage.TaskEditFromTask(t)
	if req.Prompt != "" && req.Prompt != t.Prompt {
		edit.Prompt = req.Prompt
		changed = append(changed, "prompt")
	}
	// Edits apply to the prompt as stored, not to anything the model retyped,
	// so a small change cannot drop what the edit does not name. One edit that
	// does not match exactly once skips the whole task, unchanged — a partly
	// edited prompt is a prompt nobody approved.
	if len(req.PromptEdits) > 0 || req.PromptAppend != "" {
		next, err := tools.ApplyPromptEdits(t.Prompt, req.PromptEdits, req.PromptAppend)
		if err != nil {
			return "", err
		}
		if next != t.Prompt {
			edit.Prompt = next
			changed = append(changed, promptEditDetail(req))
		}
	}
	if req.Cron != "" && req.Cron != t.Recurrence {
		// The API edit path validates the expression and refreshes the next
		// fire from it (in the task's zone); an unparseable cron stored as-is
		// would leave the row wedged as scheduled with a schedule the
		// scheduler cannot evaluate.
		loc, _ := storage.TaskLocation(t.Timezone, schedStorage.Location())
		next, err := storage.NextRun(req.Cron, loc, time.Now())
		if errors.Is(err, storage.ErrRecurrenceNeverFires) {
			return "", fmt.Errorf("%q never fires", req.Cron)
		}
		if err != nil {
			return "", fmt.Errorf("%q is not a standard 5-field cron expression", req.Cron)
		}
		edit.Recurrence = req.Cron
		edit.ScheduledFor = &next
		changed = append(changed, "schedule "+req.Cron)
	}
	if req.Model != "" && (t.Model == nil || *t.Model != req.Model) {
		m := req.Model
		edit.Model = &m
		changed = append(changed, "model "+req.Model)
	}
	if req.MaxIterations > 0 && (t.MaxIterations == nil || *t.MaxIterations != req.MaxIterations) {
		n := req.MaxIterations
		edit.MaxIterations = &n
		changed = append(changed, fmt.Sprintf("max_iterations %d", n))
	}
	// The API create/edit handlers bound a prompt's length; this path reaches
	// UpdateEditableTask without them, so an edit that empties a prompt (old =
	// the whole prompt, new = "") or an append that overflows it is refused
	// here, by the same rule, before anything is written.
	if edit.Prompt != t.Prompt {
		if err := schedmodels.ValidateTaskPromptLength(edit.Prompt); err != nil {
			return "", err
		}
	}
	if len(changed) > 0 {
		if _, err := schedStorage.UpdateEditableTask(ctx, t.ID, edit); err != nil {
			if errors.Is(err, storage.ErrTaskNotEditable) {
				return "", fmt.Errorf("task is %s and can no longer be edited", t.Status)
			}
			return "", err
		}
	}
	// Tags go through their own storage path (add/remove semantics), so they are
	// applied separately and only when asked for.
	if len(req.AddTags) > 0 || len(req.RemoveTags) > 0 {
		if _, err := schedStorage.UpdateTaskTags(ctx, t.ID, req.AddTags, req.RemoveTags); err != nil {
			return "", err
		}
		changed = append(changed, "tags")
	}
	if len(changed) == 0 {
		return "already matched the requested values", nil
	}
	return strings.Join(changed, ", "), nil
}

// promptEditDetail names an applied prompt edit in the post-approval report.
func promptEditDetail(req httpapi.TaskMutationRequest) string {
	var parts []string
	switch n := len(req.PromptEdits); {
	case n == 1:
		parts = append(parts, "1 edit")
	case n > 1:
		parts = append(parts, fmt.Sprintf("%d edits", n))
	}
	if req.PromptAppend != "" {
		parts = append(parts, "text appended")
	}
	return "prompt (" + strings.Join(parts, ", ") + ")"
}

// taskSnapshot is a task's current definition for a read-only show.
func taskSnapshot(t *schedmodels.Task, label string) httpapi.TaskSnapshot {
	snap := httpapi.TaskSnapshot{
		ID: t.ID.String(), Label: label, Status: string(t.Status),
		Cron: t.Recurrence, Timezone: t.Timezone, Tags: t.Tags, Prompt: t.Prompt,
	}
	if t.Model != nil {
		snap.Model = *t.Model
	}
	return snap
}

// taskMutationLabel is the human name for a task in the report: its title, then
// its name, then the first line of its prompt. A bare UUID tells the approving
// user nothing about what they just changed.
func taskMutationLabel(t *schedmodels.Task) string {
	if t == nil {
		return "(unknown task)"
	}
	for _, candidate := range []string{t.Title, t.Name, strings.SplitN(t.Prompt, "\n", 2)[0]} {
		if s := strings.TrimSpace(candidate); s != "" {
			if r := []rune(s); len(r) > 60 {
				return string(r[:60]) + "…"
			}
			return s
		}
	}
	return t.ID.String()
}

func taskSchedulerProvider(schedStorage *storage.Storage, budgetGate *budget.Enforcer) func(context.Context, httpapi.TaskScheduleRequest) (*httpapi.TaskScheduleResult, error) {
	return func(ctx context.Context, req httpapi.TaskScheduleRequest) (*httpapi.TaskScheduleResult, error) {
		// Per-principal rolling budget (#601 part 2): the chat path runs the
		// SAME shared gate POST /tasks and /tasks/batch run, keyed on the
		// approving user's email, BEFORE anything is created — an exhausted
		// budget surfaces in chat as the schedule_task failure text.
		if err := budgetGate.CheckCreate(ctx, schedmodels.BudgetPrincipals{User: req.RequestedBy}); err != nil {
			return nil, err
		}

		tc := schedmodels.TaskCreate{
			Title:        req.Title,
			Prompt:       req.Prompt,
			Recurrence:   req.Cron,
			ScheduledFor: req.RunAt,
			AllowNetwork: req.AllowNetwork,
		}
		if m := strings.TrimSpace(req.Model); m != "" {
			tc.Model = &m
		}
		if req.MaxIterations > 0 {
			iters := req.MaxIterations
			tc.MaxIterations = &iters
		}
		tc.ThinkingBudgetTokens = req.ThinkingBudgetTokens
		// The conversation's connector selection travels with the task
		// (ADR-0068): a scheduled run binds only its saved mcp_selection plus
		// the bundle's always-on servers, so without this a chat-created task
		// in an all-optional bundle ran with no connectors at all.
		for _, c := range req.Connectors {
			if server := strings.TrimSpace(c.Server); server != "" {
				tc.MCPSelection = append(tc.MCPSelection, schedmodels.MCPChoice{Server: server, Account: strings.TrimSpace(c.Account)})
			}
		}
		// Tag chat-originated tasks for provenance so an operator can tell at a
		// glance the task was created from a conversation rather than the API/CLI.
		// EnqueueTask does NOT run the handler's tag normalization, so we apply the
		// SAME NormalizeAndValidateTags the public POST /tasks path uses here —
		// otherwise a chat-supplied tag could violate the documented #212 format
		// (lowercase alphanumeric + '-'/'.', ≤20). The provenance tag uses a hyphen
		// (not ':') to stay inside that allowed character set.
		normalizedTags, err := schedmodels.NormalizeAndValidateTags(append(req.Tags, "source-chat"))
		if err != nil {
			return nil, err
		}
		tc.Tags = normalizedTags

		// Attribute the task to the chat user who approved it (#980): run-log
		// reads are creator-scoped, so a chat-scheduled task with no creator is
		// one its own author cannot read the transcript of. Since the one
		// identity table (#2049 M4) every enabled account has a UUID, so a
		// chat-only person's task is attributed to them too; an unknown
		// requester falls through to an unattributed task, as before.
		createdBy := accountOwnerID(ctx, schedStorage, req.RequestedBy)

		id, status, nextRunAt, err := schedStorage.EnqueueTaskAs(ctx, tc, createdBy)
		if err != nil {
			return nil, err
		}
		return &httpapi.TaskScheduleResult{
			ID:      id.String(),
			Status:  status,
			NextRun: nextRunAt,
		}, nil
	}
}

// accountOwnerID is the UUID of email's enabled account in the one identity
// table, or nil when there is none.
func accountOwnerID(ctx context.Context, schedStorage *storage.Storage, email string) *uuid.UUID {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return nil
	}
	u, err := schedStorage.GetIdentity(ctx, email)
	if err != nil || u == nil || !u.Enabled {
		return nil
	}
	id := u.ID
	return &id
}

func workerStatsProvider(schedStorage *storage.Storage) func(context.Context) (*httpapi.WorkerStats, error) {
	return func(context.Context) (*httpapi.WorkerStats, error) {
		ds, err := schedStorage.GetDashboardStats()
		if err != nil {
			return nil, err
		}
		return &httpapi.WorkerStats{
			QueuedTasks:    ds.PendingTasks,
			RunningTasks:   ds.RunningTasks,
			CompletedToday: ds.CompletedTasksToday,
			FailedToday:    ds.FailedTasksToday,
		}, nil
	}
}
