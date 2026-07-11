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

// TestValidateTaskCreate_PersonaCatalog pins the #720 create-time existence
// check: with the persona catalog wired, an unknown persona is a fail-fast
// error listing the valid names (a typo used to silently dispatch on the
// global default); a known persona still passes, and an empty persona never
// consults the catalog.
func TestValidateTaskCreate_PersonaCatalog(t *testing.T) {
	h := &Handlers{}
	h.SetPersonaCatalog(func() []string { return []string{"assistant", "security-auditor"} })
	prompt := "do the thing for the team"

	t.Run("known persona accepted", func(t *testing.T) {
		if err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt, Persona: "security-auditor"}); err != nil {
			t.Errorf("known persona should be accepted, got %v", err)
		}
	})

	t.Run("empty persona never consults the catalog", func(t *testing.T) {
		if err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt}); err != nil {
			t.Errorf("empty persona should be accepted, got %v", err)
		}
	})

	t.Run("unknown persona rejected listing valid names", func(t *testing.T) {
		err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt, Persona: "security-auditer"})
		if err == nil {
			t.Fatal("unknown persona should be rejected")
		}
		for _, want := range []string{"security-auditer", "assistant", "security-auditor"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q should mention %q", err.Error(), want)
			}
		}
	})

	t.Run("empty catalog says so", func(t *testing.T) {
		h := &Handlers{}
		h.SetPersonaCatalog(func() []string { return nil })
		err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt, Persona: "anything"})
		if err == nil || !strings.Contains(err.Error(), "no personas") {
			t.Errorf("want a 'declares no personas' error, got %v", err)
		}
	})
}
