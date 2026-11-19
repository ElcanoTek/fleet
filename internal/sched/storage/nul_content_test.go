package storage

import (
	"context"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// The create and edit paths that do not go through the HTTP handler (an
// approved chat schedule_task or manage_tasks edit, the create_task tool)
// check a task's prompt as it will be stored, without U+0000: a 0x00 byte
// cannot hide a malformed EXECUTION REQUIREMENTS marker, and a prompt of
// nothing but 0x00 bytes is empty.
func TestEnqueueAndEditCheckContentAsStored(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	ctx := context.Background()
	hidden := "Summarize the day for the team.\nEXECUTION REQUIREMENTS (JSON):\x00\n{not json"
	if _, _, _, err := store.EnqueueTask(ctx, models.TaskCreate{Prompt: hidden}); err == nil || !strings.Contains(err.Error(), "execution requirements") {
		t.Errorf("enqueue with a marker hidden by a 0x00 byte: err = %v, want the execution requirements refusal", err)
	}
	if _, _, _, err := store.EnqueueTask(ctx, models.TaskCreate{Prompt: strings.Repeat("\x00", 64)}); err == nil || err.Error() != "prompt is required" {
		t.Errorf("enqueue of a prompt of 0x00 bytes: err = %v, want prompt is required", err)
	}
	id, _, _, err := store.EnqueueTask(ctx, models.TaskCreate{Prompt: "Summarize the day for the team."})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateEditableTask(ctx, id, TaskEdit{Prompt: hidden}); err == nil || !strings.Contains(err.Error(), "execution requirements") {
		t.Errorf("edit with a marker hidden by a 0x00 byte: err = %v, want the execution requirements refusal", err)
	}
}
