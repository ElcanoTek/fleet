package handlers

import (
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// validateTaskCreate must reject a structurally-invalid worktree_config (#180)
// up front, and accept nil / valid configs. This pins that WorktreeConfig.Validate
// is actually wired into the create+edit validation path.
func TestValidateTaskCreate_WorktreeConfig(t *testing.T) {
	h := &Handlers{}
	prompt := "do the thing for the team"

	t.Run("invalid branch prefix rejected", func(t *testing.T) {
		err := h.validateTaskCreate(&models.TaskCreate{
			Prompt:         prompt,
			WorktreeConfig: &models.WorktreeConfig{Enabled: true, BranchPrefix: "bad prefix"},
		})
		if err == nil || !strings.Contains(err.Error(), "worktree_config") {
			t.Fatalf("expected a worktree_config error, got %v", err)
		}
	})

	t.Run("negative cleanup delay rejected", func(t *testing.T) {
		err := h.validateTaskCreate(&models.TaskCreate{
			Prompt:         prompt,
			WorktreeConfig: &models.WorktreeConfig{Enabled: true, CleanupDelaySeconds: -5},
		})
		if err == nil || !strings.Contains(err.Error(), "worktree_config") {
			t.Fatalf("expected a worktree_config error, got %v", err)
		}
	})

	t.Run("valid config accepted", func(t *testing.T) {
		if err := h.validateTaskCreate(&models.TaskCreate{
			Prompt:         prompt,
			WorktreeConfig: &models.WorktreeConfig{Enabled: true, BranchPrefix: "fleet/task-", BaseBranch: "main", AutoCleanup: true},
		}); err != nil {
			t.Fatalf("valid worktree_config should be accepted, got %v", err)
		}
	})

	t.Run("nil config accepted (shared workspace)", func(t *testing.T) {
		if err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt}); err != nil {
			t.Fatalf("nil worktree_config should be accepted, got %v", err)
		}
	})
}
