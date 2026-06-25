package handlers

import (
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// validateTaskCreate must reject a runtime flavor the deployment doesn't offer
// (e.g. native-inprocess when FLEET_ENABLE_INPROCESS_LOOP is off, #159) up front
// rather than silently falling back at dispatch.
func TestValidateTaskCreate_RuntimeFlavorGate(t *testing.T) {
	h := &Handlers{
		// Only native-acp is selectable on this deployment.
		runtimeSelectable: func(name string) bool { return name == "native-acp" },
	}
	prompt := "do the thing for the team"

	t.Run("gated flavor rejected", func(t *testing.T) {
		err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt, RuntimeFlavor: "native-inprocess"})
		if err == nil || !strings.Contains(err.Error(), "not available") {
			t.Fatalf("expected a not-available error, got %v", err)
		}
	})

	t.Run("selectable flavor accepted", func(t *testing.T) {
		if err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt, RuntimeFlavor: "native-acp"}); err != nil {
			t.Fatalf("native-acp should be accepted, got %v", err)
		}
	})

	t.Run("empty flavor accepted (uses default)", func(t *testing.T) {
		if err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt}); err != nil {
			t.Fatalf("empty flavor should be accepted, got %v", err)
		}
	})

	t.Run("no validator wired = no runtime gate", func(t *testing.T) {
		h2 := &Handlers{}
		if err := h2.validateTaskCreate(&models.TaskCreate{Prompt: prompt, RuntimeFlavor: "anything"}); err != nil {
			t.Fatalf("with no validator, any flavor should pass, got %v", err)
		}
	})
}
