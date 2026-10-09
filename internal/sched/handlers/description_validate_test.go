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
	h := newValidateTestHandlers()
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

// TestValidateTaskCreate_PromptMaxBytes pins the 250000-byte prompt cap (MOC
// parity for 90-deal Manifest batches). The literal is deliberate: raising or
// lowering the cap must be a conscious edit here too. The cap counts BYTES.
func TestValidateTaskCreate_PromptMaxBytes(t *testing.T) {
	h := newValidateTestHandlers()

	if err := h.validateTaskCreate(&models.TaskCreate{Prompt: strings.Repeat("a", 250000)}); err != nil {
		t.Fatalf("250000-byte prompt should be accepted, got %v", err)
	}
	err := h.validateTaskCreate(&models.TaskCreate{Prompt: strings.Repeat("a", 250001)})
	if err == nil || !strings.Contains(err.Error(), "prompt cannot exceed 250000 bytes") {
		t.Fatalf("250001-byte prompt should be rejected with the byte limit, got %v", err)
	}
	// 125000 two-byte runes = 250000 bytes (accepted); one more rune = 250002.
	if err := h.validateTaskCreate(&models.TaskCreate{Prompt: strings.Repeat("é", 125000)}); err != nil {
		t.Fatalf("125000 two-byte runes (250000 bytes) should be accepted, got %v", err)
	}
	if err := h.validateTaskCreate(&models.TaskCreate{Prompt: strings.Repeat("é", 125001)}); err == nil {
		t.Fatal("125001 two-byte runes (250002 bytes) should be rejected: the cap counts bytes")
	}
}
