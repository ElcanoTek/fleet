package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

func parseTaskTrackerResult(t *testing.T, result string) taskTrackerResult {
	t.Helper()
	var parsed taskTrackerResult
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("expected JSON task tracker result, got parse error: %v", err)
	}
	return parsed
}

func setupTaskTracker(t *testing.T) *taskTracker {
	t.Helper()
	return &taskTracker{tasks: []Task{}}
}

func TestTaskTrackerToolView(t *testing.T) {
	tool := setupTaskTracker(t)

	// View empty task list
	result, err := tool.run(TaskTrackerParams{
		Command: "view",
	})

	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	parsed := parseTaskTrackerResult(t, result)
	if !strings.Contains(parsed.Output, "No tasks") {
		t.Errorf("Expected 'No tasks' message for empty list, got %s", result)
	}
}

func TestTaskTrackerToolPlan(t *testing.T) {
	tool := setupTaskTracker(t)

	// Create task list
	taskList := []Task{
		{ID: "1", Title: "Task 1", Status: "todo", Notes: "First task"},
		{ID: "2", Title: "Task 2", Status: "in_progress"},
		{ID: "3", Title: "Task 3", Status: "done"},
	}

	result, err := tool.run(TaskTrackerParams{
		Command:  "plan",
		TaskList: taskList,
	})

	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	parsed := parseTaskTrackerResult(t, result)
	if !strings.Contains(parsed.Output, "Task 1") {
		t.Errorf("Expected to find 'Task 1', got %s", result)
	}

	if !strings.Contains(parsed.Output, "3 total") {
		t.Errorf("Expected to find '3 total', got %s", result)
	}

	if parsed.Summary.Todo != 1 {
		t.Errorf("Expected to find '1 todo', got %s", result)
	}

	if parsed.Summary.InProgress != 1 {
		t.Errorf("Expected to find '1 in progress', got %s", result)
	}

	if parsed.Summary.Done != 1 {
		t.Errorf("Expected to find '1 done', got %s", result)
	}
}

func TestTaskTrackerToolInvalidStatus(t *testing.T) {
	tool := setupTaskTracker(t)

	taskList := []Task{
		{ID: "1", Title: "Task 1", Status: "invalid_status"},
	}

	_, err := tool.run(TaskTrackerParams{
		Command:  "plan",
		TaskList: taskList,
	})

	if err == nil {
		t.Errorf("Expected error for invalid status, got nil")
	}

	if !strings.Contains(err.Error(), "invalid status") {
		t.Errorf("Expected 'invalid status' error, got %v", err)
	}
}

func TestTaskTrackerToolDuplicateID(t *testing.T) {
	tool := setupTaskTracker(t)

	taskList := []Task{
		{ID: "1", Title: "Task 1", Status: "todo"},
		{ID: "1", Title: "Task 2", Status: "todo"},
	}

	_, err := tool.run(TaskTrackerParams{
		Command:  "plan",
		TaskList: taskList,
	})

	if err == nil {
		t.Errorf("Expected error for duplicate ID, got nil")
	}

	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("Expected 'duplicate' error, got %v", err)
	}
}

func TestTaskTrackerToolMissingFields(t *testing.T) {
	tool := setupTaskTracker(t)

	// Missing title
	taskList := []Task{
		{ID: "1", Status: "todo"},
	}

	_, err := tool.run(TaskTrackerParams{
		Command:  "plan",
		TaskList: taskList,
	})

	if err == nil {
		t.Errorf("Expected error for missing title, got nil")
	}
}

func TestTaskTrackerToolPersistence(t *testing.T) {
	tool := setupTaskTracker(t)

	// Create initial task list
	_, err := tool.run(TaskTrackerParams{
		Command:  "plan",
		TaskList: []Task{{ID: "1", Title: "Task 1", Status: "todo"}},
	})

	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	// Update task list
	result, err := tool.run(TaskTrackerParams{
		Command: "plan",
		TaskList: []Task{
			{ID: "1", Title: "Task 1", Status: "done"},
			{ID: "2", Title: "Task 2", Status: "in_progress"},
		},
	})

	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	parsed := parseTaskTrackerResult(t, result)
	if !strings.Contains(parsed.Output, "2 total") {
		t.Errorf("Expected to find '2 total', got %s", result)
	}

	if !strings.Contains(parsed.Output, "Task 2") {
		t.Errorf("Expected to find 'Task 2', got %s", result)
	}
}

func TestTaskTrackerToolPreservesNotesOnReplan(t *testing.T) {
	tool := setupTaskTracker(t)

	if _, err := tool.run(TaskTrackerParams{
		Command:  "plan",
		TaskList: []Task{{ID: "1", Title: "Retrieve current Magnite report", Status: "todo", Notes: "Use search_emails result emails_0_s3_key for follow-up get_email."}},
	}); err != nil {
		t.Fatalf("initial plan failed: %v", err)
	}

	result, err := tool.run(TaskTrackerParams{
		Command:  "plan",
		TaskList: []Task{{ID: "1", Title: "Retrieve current Magnite report", Status: "in_progress"}},
	})
	if err != nil {
		t.Fatalf("replan failed: %v", err)
	}
	parsed := parseTaskTrackerResult(t, result)
	if !strings.Contains(parsed.Output, "Use search_emails result emails_0_s3_key") {
		t.Fatalf("expected notes to be preserved, got %s", result)
	}
}

func TestTaskTrackerToolStructuredFields(t *testing.T) {
	tool := setupTaskTracker(t)

	result, err := tool.run(TaskTrackerParams{
		Command: "plan",
		TaskList: []Task{
			{ID: "1", Title: "Task 1", Status: "done"},
			{ID: "2", Title: "Task 2", Status: "in_progress"},
		},
	})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	parsed := parseTaskTrackerResult(t, result)
	if parsed.Command != "plan" {
		t.Fatalf("expected command=plan, got %q", parsed.Command)
	}
	if parsed.ActiveTask != "Task 2" {
		t.Fatalf("expected active task Task 2, got %q", parsed.ActiveTask)
	}
	if len(parsed.Tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(parsed.Tasks))
	}
	if parsed.ExecutionTimeMs < 0 {
		t.Fatalf("expected non-negative execution time, got %d", parsed.ExecutionTimeMs)
	}
}
