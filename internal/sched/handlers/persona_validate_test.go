package handlers

import (
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestValidateTaskCreate_Persona pins the #221 persona override validation:
// empty/clean names accepted, path-bearing names rejected.
func TestValidateTaskCreate_Persona(t *testing.T) {
	h := &Handlers{}
	prompt := "do the thing for the team"

	t.Run("empty and clean names accepted", func(t *testing.T) {
		for _, ok := range []string{"", "security-auditor", "tech_writer", "assistant"} {
			if err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt, Persona: ok}); err != nil {
				t.Errorf("persona %q should be accepted, got %v", ok, err)
			}
		}
	})

	t.Run("path-bearing names rejected", func(t *testing.T) {
		for _, bad := range []string{"../etc/passwd", "a/b", "..", `x\y`, "../assistant"} {
			err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt, Persona: bad})
			if err == nil || !strings.Contains(err.Error(), "persona") {
				t.Errorf("persona %q should be rejected with a persona error, got %v", bad, err)
			}
		}
	})
}
