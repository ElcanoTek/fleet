package models

import "testing"

func TestTaskToCreate(t *testing.T) {
	model := "anthropic/claude-sonnet-4-5"
	src := &Task{
		Prompt:        "do the work",
		Model:         &model,
		Priority:      7,
		Tags:          []string{"nightly"},
		Description:   "docs",
		RuntimeFlavor: "native-acp",
		Recurrence:    "0 9 * * *",
		Timezone:      "America/New_York",
		AllowNetwork:  true,
		MaxRetries:    3,
		RetryPolicy:   &RetryPolicy{Backoff: BackoffFixed},
		// Runtime-only fields that must NOT carry:
		Status:       TaskStatusSuccess,
		AttemptCount: 2,
	}
	tc := TaskToCreate(src)

	if tc.Prompt != "do the work" || tc.Model == nil || *tc.Model != model || tc.Priority != 7 ||
		tc.RuntimeFlavor != "native-acp" || tc.Recurrence != "0 9 * * *" || tc.Timezone != "America/New_York" ||
		!tc.AllowNetwork || tc.Description != "docs" || len(tc.Tags) != 1 || tc.RetryPolicy == nil {
		t.Errorf("TaskToCreate did not copy create fields: %+v", tc)
	}
	if tc.MaxRetries == nil || *tc.MaxRetries != 3 {
		t.Errorf("MaxRetries should round-trip as a pointer to 3, got %v", tc.MaxRetries)
	}
	// NewTask over the recipe must mint a fresh pending task with a new ID.
	nt := NewTask(tc)
	if nt.ID == src.ID {
		t.Error("re-created task must have a fresh ID")
	}
	if nt.Status == TaskStatusSuccess {
		t.Error("re-created task must not inherit the source's terminal status")
	}
}
