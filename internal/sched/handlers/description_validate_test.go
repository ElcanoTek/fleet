package handlers

import (
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestValidateTaskCreate_Description pins the #281 length cap: a description up
// to maxTaskDescriptionChars runes is accepted; one rune over is rejected; empty
// is fine. Counting is rune-based, so multibyte text is not penalized by bytes.
func TestValidateTaskCreate_Description(t *testing.T) {
	h := &Handlers{}
	prompt := "do the thing for the team"

	t.Run("empty description accepted", func(t *testing.T) {
		if err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt}); err != nil {
			t.Fatalf("empty description should be accepted, got %v", err)
		}
	})

	t.Run("at limit accepted", func(t *testing.T) {
		desc := strings.Repeat("é", maxTaskDescriptionChars) // multibyte: 2 bytes each, 1 rune each
		if err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt, Description: desc}); err != nil {
			t.Fatalf("description at the rune limit should be accepted, got %v", err)
		}
	})

	t.Run("over limit rejected", func(t *testing.T) {
		desc := strings.Repeat("a", maxTaskDescriptionChars+1)
		err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt, Description: desc})
		if err == nil || !strings.Contains(err.Error(), "description") {
			t.Fatalf("over-limit description should be rejected with a description error, got %v", err)
		}
	})
}
