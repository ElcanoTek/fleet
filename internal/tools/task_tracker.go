package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
)

const (
	StatusTodo       = "todo"
	StatusInProgress = "in_progress"
	StatusDone       = "done"
)

// Task represents a structured task with status tracking
type Task struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"` // todo, in_progress, done
	Notes  string `json:"notes,omitempty"`
}

// taskTracker holds internal state for the task tracker tool.
type taskTracker struct {
	tasks []Task
	mu    sync.Mutex
}

type taskTrackerSummary struct {
	Total      int `json:"total"`
	Todo       int `json:"todo"`
	InProgress int `json:"in_progress"`
	Done       int `json:"done"`
}

type taskTrackerResult struct {
	Status          string             `json:"status"`
	Command         string             `json:"command"`
	Output          string             `json:"output"`
	Tasks           []Task             `json:"tasks,omitempty"`
	Summary         taskTrackerSummary `json:"summary"`
	ActiveTask      string             `json:"active_task,omitempty"`
	ExecutionTimeMs int64              `json:"execution_time_ms"`
}

// TaskTrackerParams are the typed parameters for the task_tracker tool.
type TaskTrackerParams struct {
	Command  string `json:"command" description:"The command to execute. 'view' shows the current task list. 'plan' creates or updates the task list."`
	TaskList []Task `json:"task_list,omitempty" description:"The full task list. Required for 'plan' command."`
}

// NewTaskTrackerTool creates a fantasy.AgentTool for structured task management.
func NewTaskTrackerTool() fantasy.AgentTool {
	t := &taskTracker{tasks: []Task{}}
	return fantasy.NewAgentTool("task_tracker", taskTrackerDescription,
		func(_ context.Context, params TaskTrackerParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			result, err := t.run(params)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(result), nil
		})
}

const taskTrackerDescription = `Structured task management for complex, multi-phase workflows.

This tool provides systematic tracking of work items, progress monitoring, and efficient
organization of development activities.

## Commands
- **view**: Display the current task list with status
- **plan**: Create or update the task list with new tasks

## Task Status Values
- **todo**: Not yet initiated
- **in_progress**: Currently active (only one at a time)
- **done**: Successfully completed

## Best Practices
1. Update status dynamically as work progresses
2. Mark completion immediately upon task finish
3. Limit active work to ONE task at a time
4. Write precise, actionable task descriptions
5. Re-run plan whenever the active task changes
6. Before the final user-facing answer, update the list so completed tasks are done and the next unfinished task is in_progress only if work is actually continuing`

func (t *taskTracker) run(params TaskTrackerParams) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	start := time.Now()

	switch params.Command {
	case "view":
		return t.renderResult(params.Command, time.Since(start).Milliseconds()), nil
	case "plan":
		if len(params.TaskList) == 0 {
			return "", fmt.Errorf("task_list is required for 'plan' command")
		}
		if err := t.validateTasks(params.TaskList); err != nil {
			return "", err
		}
		preserveTaskNotes(t.tasks, params.TaskList)
		t.tasks = params.TaskList
		return t.renderResult(params.Command, time.Since(start).Milliseconds()), nil
	default:
		return "", fmt.Errorf("unknown command: %s (must be 'view' or 'plan')", params.Command)
	}
}

func (t *taskTracker) renderResult(command string, executionTimeMs int64) string {
	output, summary, activeTask, tasks := t.viewTasks()
	result := taskTrackerResult{
		Status:          "success",
		Command:         command,
		Output:          output,
		Tasks:           tasks,
		Summary:         summary,
		ActiveTask:      activeTask,
		ExecutionTimeMs: executionTimeMs,
	}
	jsonBytes, err := json.Marshal(result)
	if err != nil {
		return output
	}
	return string(jsonBytes)
}

func (t *taskTracker) viewTasks() (string, taskTrackerSummary, string, []Task) {
	if len(t.tasks) == 0 {
		return "No tasks in the task list. Use 'plan' command to create tasks.", taskTrackerSummary{}, "", nil
	}

	var result strings.Builder
	result.WriteString("Task List:\n")
	result.WriteString("==========\n\n")

	// Count tasks by status
	todoCount := 0
	inProgressCount := 0
	doneCount := 0

	for _, task := range t.tasks {
		switch task.Status {
		case StatusTodo:
			todoCount++
		case StatusInProgress:
			inProgressCount++
		case StatusDone:
			doneCount++
		}
	}

	fmt.Fprintf(&result, "Summary: %d total (%d todo, %d in progress, %d done)\n\n", len(t.tasks), todoCount, inProgressCount, doneCount)

	// Display tasks
	for _, task := range t.tasks {
		statusIcon := getStatusIcon(task.Status)
		fmt.Fprintf(&result, "%s [%s] %s\n", statusIcon, task.ID, task.Title)
		if task.Notes != "" {
			fmt.Fprintf(&result, "   Notes: %s\n", task.Notes)
		}
	}
	summary := taskTrackerSummary{Total: len(t.tasks), Todo: todoCount, InProgress: inProgressCount, Done: doneCount}
	activeTask := ""
	tasks := append([]Task(nil), t.tasks...)
	for _, task := range t.tasks {
		if task.Status == StatusInProgress {
			activeTask = task.Title
			break
		}
	}

	return result.String(), summary, activeTask, tasks
}

func (t *taskTracker) validateTasks(tasks []Task) error {
	if len(tasks) == 0 {
		return fmt.Errorf("task_list cannot be empty")
	}

	// Check for duplicate IDs
	seenIDs := make(map[string]bool)
	inProgressCount := 0

	for i, task := range tasks {
		if task.ID == "" {
			return fmt.Errorf("task %d: id is required", i+1)
		}
		if task.Title == "" {
			return fmt.Errorf("task %d: title is required", i+1)
		}
		if task.Status == "" {
			return fmt.Errorf("task %d: status is required", i+1)
		}

		// Validate status
		if task.Status != StatusTodo && task.Status != StatusInProgress && task.Status != StatusDone {
			return fmt.Errorf("task %d: invalid status '%s' (must be 'todo', 'in_progress', or 'done')", i+1, task.Status)
		}
		// Check for duplicate IDs
		if seenIDs[task.ID] {
			return fmt.Errorf("duplicate task ID: %s", task.ID)
		}
		seenIDs[task.ID] = true

		// Count in_progress tasks
		if task.Status == StatusInProgress {
			inProgressCount++
		}
	}

	// Warn if multiple tasks are in_progress (but don't error)
	// if inProgressCount > 1 {
	// 	// This is just a warning in the description, not enforced
	// }

	return nil
}

func preserveTaskNotes(existing, updated []Task) {
	if len(existing) == 0 || len(updated) == 0 {
		return
	}

	byID := make(map[string]Task, len(existing))
	byTitle := make(map[string]Task, len(existing))
	for _, task := range existing {
		byID[task.ID] = task
		byTitle[normalizeTaskTitle(task.Title)] = task
	}

	for i := range updated {
		candidate := updated[i]
		if strings.TrimSpace(updated[i].Notes) == "" {
			if prior, ok := byID[candidate.ID]; ok && strings.TrimSpace(prior.Notes) != "" {
				updated[i].Notes = prior.Notes
			} else if prior, ok := byTitle[normalizeTaskTitle(candidate.Title)]; ok && strings.TrimSpace(prior.Notes) != "" {
				updated[i].Notes = prior.Notes
			}
		}
	}
}

func normalizeTaskTitle(title string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(title))), " ")
}

func getStatusIcon(status string) string {
	switch status {
	case StatusTodo:
		return "[ ]"
	case StatusInProgress:
		return "[~]"
	case StatusDone:
		return "[✓]"
	default:
		return "[?]"
	}
}
