package handlers

import (
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestValidateTaskCreate_Tags pins that validateTaskCreate normalizes tags in
// place (lowercase/dedupe) and rejects invalid ones — on the create+edit path.
func TestValidateTaskCreate_Tags(t *testing.T) {
	h := &Handlers{}
	prompt := "do the thing for the team"

	t.Run("normalizes in place", func(t *testing.T) {
		tc := &models.TaskCreate{Prompt: prompt, Tags: []string{"Nightly", "PROD", "nightly"}}
		if err := h.validateTaskCreate(tc); err != nil {
			t.Fatalf("valid tags rejected: %v", err)
		}
		if len(tc.Tags) != 2 || tc.Tags[0] != "nightly" || tc.Tags[1] != "prod" {
			t.Errorf("tags should be normalized in place, got %v", tc.Tags)
		}
	})

	t.Run("invalid tag rejected", func(t *testing.T) {
		err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt, Tags: []string{"has space"}})
		if err == nil || !strings.Contains(err.Error(), "tags") {
			t.Fatalf("invalid tag should be rejected with a tags error, got %v", err)
		}
	})

	t.Run("no tags accepted", func(t *testing.T) {
		if err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt}); err != nil {
			t.Fatalf("no tags should be accepted, got %v", err)
		}
	})
}
