package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"
)

// ScheduleTaskToolName is the canonical name of the interactive schedule_task tool.
const ScheduleTaskToolName = "schedule_task"

// ScheduleTaskParams is the agent-facing input schema for schedule_task (#239).
// It maps to a subset of models.TaskCreate fields; the chat→orchestrator
// translation happens in the approval-execution seam (internal/httpapi), so this
// package stays free of a sched dependency.
//
// Supply EITHER cron (recurring) OR run_at (one-time future run), never both.
// Omitting both creates a task that runs as soon as a worker is free.
type ScheduleTaskParams struct {
	Name          string   `json:"name,omitempty" description:"Short human-readable label for the task, shown in the scheduler UI. Optional but recommended; must be unique across tasks."`
	Prompt        string   `json:"prompt" description:"Full instructions for what the scheduled agent should do each run. Required."`
	Cron          string   `json:"cron,omitempty" description:"Standard 5-field cron expression for a RECURRING task, e.g. '0 9 * * MON-FRI'. Omit for one-time runs. Mutually exclusive with run_at."`
	RunAt         string   `json:"run_at,omitempty" description:"RFC3339 / ISO-8601 datetime for a ONE-TIME future run, e.g. '2026-07-01T09:00:00Z'. Omit for recurring tasks. Mutually exclusive with cron."`
	Model         string   `json:"model,omitempty" description:"Optional model slug override for the scheduled task. Defaults to the orchestrator's configured model."`
	MaxIterations int      `json:"max_iterations,omitempty" description:"Optional cap on agent steps per run. Omit for the orchestrator default."`
	AllowNetwork  bool     `json:"allow_network,omitempty" description:"Whether the scheduled task's sandbox keeps outbound network egress. Default false (sealed). Set true only when the task genuinely needs to reach the network."`
	Tags          []string `json:"tags,omitempty" description:"Optional tags for organizing/filtering the task in the scheduler."`
}

const scheduleTaskDescription = `Creates a new SCHEDULED task in the Fleet orchestrator (the Operations Center), so work the user wants run later — or on a repeating cadence — runs unattended without them leaving chat.

IMPORTANT — translate the user's natural-language schedule to cron yourself BEFORE calling this tool:
  "every weekday at 9am"     → cron = "0 9 * * MON-FRI"
  "every day at midnight"    → cron = "0 0 * * *"
  "every Monday at 8:30"     → cron = "30 8 * * MON"
  "once tomorrow at 3pm UTC" → run_at = "<tomorrow's ISO-8601 datetime>"

Supply EITHER cron (recurring) OR run_at (one-time), never both. Omit both only if the user wants it to run once, immediately. If the user's phrasing is ambiguous (no time, unclear cadence, unclear timezone), ASK a clarifying question instead of guessing.

This is a CRITICAL action: the user sees an approval card (task name, prompt preview, schedule, estimated run frequency) and must click Approve before the task is created. After approval the tool returns the new task's id and a pointer to view it in the Operations Center. Do NOT call this tool to do work right now — it enqueues a NEW independent scheduled task; for work in the current conversation, just do it directly.`

// NewScheduleTaskTool returns the schedule_task tool. Its Run is an explicit
// error because the interactive orchestration layer intercepts every call,
// stages it for user approval, and performs the actual orchestrator task
// creation on approval (mirrors preview_email). If Run ever fires, the gate is
// mis-wired.
func NewScheduleTaskTool() fantasy.AgentTool {
	return fantasy.NewAgentTool(ScheduleTaskToolName, scheduleTaskDescription,
		func(_ context.Context, _ ScheduleTaskParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextErrorResponse(
				"schedule_task was executed directly — orchestration should have staged it for user approval. This is a bug.",
			), fmt.Errorf("schedule_task bypass")
		})
}

// Validate checks a schedule_task payload for the constraints that do NOT need a
// cron parser or storage: a non-empty prompt, a mutually-exclusive cron/run_at,
// and a parseable run_at. Cron-expression validity is intentionally left to the
// storage create path (storage.EnqueueTask), the single source of truth, so the
// two cannot drift. Returns an agent-readable error.
func (p ScheduleTaskParams) Validate() error {
	if strings.TrimSpace(p.Prompt) == "" {
		return fmt.Errorf("schedule_task requires a non-empty prompt")
	}
	cron := strings.TrimSpace(p.Cron)
	runAt := strings.TrimSpace(p.RunAt)
	if cron != "" && runAt != "" {
		return fmt.Errorf("schedule_task: supply EITHER cron (recurring) OR run_at (one-time), not both")
	}
	if runAt != "" {
		if _, err := time.Parse(time.RFC3339, runAt); err != nil {
			return fmt.Errorf("schedule_task: run_at %q is not a valid RFC3339 datetime (e.g. 2026-07-01T09:00:00Z): %w", runAt, err)
		}
	}
	return nil
}

// RunAtTime returns the parsed one-time run instant (UTC) and whether run_at was
// set. A set-but-unparseable run_at returns ok=false; callers should Validate
// first to surface the parse error.
func (p ScheduleTaskParams) RunAtTime() (t time.Time, ok bool) {
	runAt := strings.TrimSpace(p.RunAt)
	if runAt == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339, runAt)
	if err != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}
