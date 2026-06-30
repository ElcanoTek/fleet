package storage

import (
	"context"
	"fmt"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/google/uuid"
)

// CreateTrigger persists a webhook trigger.
func (s *Storage) CreateTrigger(ctx context.Context, t *models.TaskTrigger) error {
	return s.db.CreateTrigger(ctx, t)
}

// GetTriggerBySlug looks up a trigger by slug (used by the webhook handler).
func (s *Storage) GetTriggerBySlug(ctx context.Context, slug string) (*models.TaskTrigger, error) {
	return s.db.GetTriggerBySlug(ctx, slug)
}

// GetTrigger looks up a trigger by ID.
func (s *Storage) GetTrigger(ctx context.Context, id uuid.UUID) (*models.TaskTrigger, error) {
	return s.db.GetTrigger(ctx, id)
}

// ListTriggers returns triggers, optionally scoped to one task.
func (s *Storage) ListTriggers(ctx context.Context, taskID *uuid.UUID) ([]*models.TaskTrigger, error) {
	return s.db.ListTriggers(ctx, taskID)
}

// DeleteTrigger removes a trigger by ID.
func (s *Storage) DeleteTrigger(ctx context.Context, id uuid.UUID) (bool, error) {
	return s.db.DeleteTrigger(ctx, id)
}

// RotateTriggerSecret replaces a trigger's HMAC secret.
func (s *Storage) RotateTriggerSecret(ctx context.Context, id uuid.UUID, secret string) (bool, error) {
	return s.db.RotateTriggerSecret(ctx, id, secret)
}

// SpawnWebhookRun creates one fresh, immediately-claimable run cloned from the
// trigger's template task, with the rendered prompt substituted (#177). The new
// run is a one-shot cron-type task (no schedule, no recurrence) so the worker
// pool claims it at once; the template itself stays inert. Returns the new run's
// ID.
func (s *Storage) SpawnWebhookRun(ctx context.Context, trigger *models.TaskTrigger, prompt string) (uuid.UUID, error) {
	template, err := s.db.GetTask(ctx, trigger.TaskID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("load template task: %w", err)
	}

	// An empty rendered prompt (no prompt_template configured) falls back to the
	// template task's own prompt, so a trigger can be a pure fire-the-task signal.
	if prompt == "" {
		prompt = template.Prompt
	}

	// Clone the template's runtime configuration. Deliberately drop Recurrence /
	// ScheduledFor / TriggerType so the spawned run is a normal one-shot task that
	// runs now rather than another inert webhook template.
	run := models.NewTask(models.TaskCreate{
		Prompt:                 prompt,
		Model:                  template.Model,
		FallbackModel:          template.FallbackModel,
		MaxIterations:          template.MaxIterations,
		MCPSelection:           template.MCPSelection,
		Priority:               template.Priority,
		InstructionSelfImprove: template.InstructionSelfImprove,
		AllowNetwork:           template.AllowNetwork,
		AllowDelegation:        template.AllowDelegation,
		Files:                  template.Files,
		MaxRetries:             &template.MaxRetries,
		Timezone:               template.Timezone,
	})
	run.CreatedBy = template.CreatedBy
	// Carry the originating API key forward so webhook-spawned run cost keeps
	// counting against the template owner's spending caps.
	run.CreatedByKeyID = template.CreatedByKeyID

	if err := s.db.AddTask(ctx, run); err != nil {
		return uuid.Nil, fmt.Errorf("create webhook run: %w", err)
	}
	return run.ID, nil
}
