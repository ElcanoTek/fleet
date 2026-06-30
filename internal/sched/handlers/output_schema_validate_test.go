package handlers

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// validateTaskCreate must reject a malformed output_schema (#244) at create time
// so the task author sees the error before the task is ever persisted, and accept
// nil / valid schemas. Pins that structuredoutput.ValidateSchema is wired into
// the create+edit validation path.
func TestValidateTaskCreate_OutputSchema(t *testing.T) {
	h := &Handlers{}
	prompt := "produce a structured report for the team"

	t.Run("malformed schema rejected", func(t *testing.T) {
		err := h.validateTaskCreate(&models.TaskCreate{
			Prompt:       prompt,
			OutputSchema: json.RawMessage(`{"type": "object", "properties":`), // truncated JSON
		})
		if err == nil || !strings.Contains(err.Error(), "output_schema") {
			t.Fatalf("expected an output_schema error, got %v", err)
		}
	})

	t.Run("non-object schema rejected", func(t *testing.T) {
		err := h.validateTaskCreate(&models.TaskCreate{
			Prompt:       prompt,
			OutputSchema: json.RawMessage(`["not", "an", "object"]`),
		})
		if err == nil || !strings.Contains(err.Error(), "output_schema") {
			t.Fatalf("expected an output_schema error, got %v", err)
		}
	})

	t.Run("valid schema accepted", func(t *testing.T) {
		if err := h.validateTaskCreate(&models.TaskCreate{
			Prompt:       prompt,
			OutputSchema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`),
		}); err != nil {
			t.Fatalf("valid output_schema should be accepted, got %v", err)
		}
	})

	t.Run("nil schema accepted (free-form text mode)", func(t *testing.T) {
		if err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt}); err != nil {
			t.Fatalf("nil output_schema should be accepted, got %v", err)
		}
	})
}
