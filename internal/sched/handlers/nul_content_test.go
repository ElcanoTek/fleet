package handlers

import (
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// A task's title, prompt and description lose U+0000 before any check, so
// every check sees what will be stored: a 0x00 byte cannot hide a malformed
// EXECUTION REQUIREMENTS marker, and a prompt of nothing but 0x00 bytes is
// empty.
func TestValidateTaskCreateChecksContentAsStored(t *testing.T) {
	t.Parallel()
	h := &Handlers{}
	hidden := &models.TaskCreate{Prompt: "Summarize the day for the team.\nEXECUTION REQUIREMENTS (JSON):\x00\n{not json"}
	if err := h.validateTaskCreate(hidden); err == nil || !strings.Contains(err.Error(), "execution requirements") {
		t.Errorf("a marker hidden by a 0x00 byte: err = %v, want the execution requirements refusal", err)
	}
	empty := &models.TaskCreate{Prompt: strings.Repeat("\x00", 64)}
	if err := h.validateTaskCreate(empty); err == nil || err.Error() != "prompt is required" {
		t.Errorf("a prompt of 0x00 bytes: err = %v, want prompt is required", err)
	}
}
