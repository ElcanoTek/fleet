package handlers

import (
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestValidateTaskCreate_RetryPolicy pins that validateTaskCreate accepts a nil
// or valid retry policy and rejects a structurally-broken one (#201).
func TestValidateTaskCreate_RetryPolicy(t *testing.T) {
	h := &Handlers{}
	prompt := "do the thing for the team"

	t.Run("nil accepted", func(t *testing.T) {
		if err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt}); err != nil {
			t.Fatalf("nil retry_policy should be accepted, got %v", err)
		}
	})

	t.Run("valid accepted", func(t *testing.T) {
		rp := &models.RetryPolicy{Backoff: models.BackoffExponential, InitialDelaySeconds: 60, MaxDelaySeconds: 3600, RetryOn: []string{models.FailureTransient}}
		if err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt, RetryPolicy: rp}); err != nil {
			t.Fatalf("valid retry_policy should be accepted, got %v", err)
		}
	})

	t.Run("invalid backoff rejected", func(t *testing.T) {
		err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt, RetryPolicy: &models.RetryPolicy{Backoff: "linear"}})
		if err == nil || !strings.Contains(err.Error(), "retry_policy") {
			t.Fatalf("invalid backoff should be rejected with a retry_policy error, got %v", err)
		}
	})

	t.Run("unknown failure class rejected", func(t *testing.T) {
		err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt, RetryPolicy: &models.RetryPolicy{RetryOn: []string{"bogus"}}})
		if err == nil || !strings.Contains(err.Error(), "retry_policy") {
			t.Fatalf("unknown failure class should be rejected, got %v", err)
		}
	})
}
