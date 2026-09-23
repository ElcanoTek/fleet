package storage

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// malformedRequirementsPrompt carries the #1601 shape: a server list entry that
// is two names joined with " + ", which fails every dispatch.
const malformedRequirementsPrompt = "Refresh the page.\n\nEXECUTION REQUIREMENTS (JSON):\n" +
	`{"mcp_servers":["files + file_helpers","pages"],"required_tools":["mcp_pages_get_page_data"]}`

// Every create path — POST /tasks, an approved schedule_task, the scheduled-run
// create_task tool — and every in-place edit (an approved manage_tasks prompt
// change) reaches these seams, so they refuse a malformed EXECUTION
// REQUIREMENTS line themselves (#1601), before any database work: a nil-DB
// Storage proves the check runs first.
func TestStorageSeamsRefuseMalformedRequirements(t *testing.T) {
	s := &Storage{}
	if _, _, _, err := s.EnqueueTaskAs(context.Background(), models.TaskCreate{Prompt: malformedRequirementsPrompt}, nil); err == nil || !strings.Contains(err.Error(), "files + file_helpers") {
		t.Fatalf("EnqueueTaskAs must refuse the malformed line and name it, got %v", err)
	}
	if _, err := s.UpdateEditableTask(context.Background(), uuid.New(), TaskEdit{Prompt: malformedRequirementsPrompt}); err == nil || !strings.Contains(err.Error(), "files + file_helpers") {
		t.Fatalf("UpdateEditableTask must refuse the malformed line and name it, got %v", err)
	}
}
