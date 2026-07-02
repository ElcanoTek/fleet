package storage

import (
	"context"
	"fmt"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/google/uuid"
)

// CreateTrigger persists a webhook/email trigger.
func (s *Storage) CreateTrigger(ctx context.Context, t *models.TaskTrigger) error {
	return s.db.CreateTrigger(ctx, t)
}

// GetTriggerBySlug looks up a trigger by slug (used by the webhook/email handlers).
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

// RecordTriggerEvent records one accepted inbound event, enforcing idempotency
// (a duplicate (trigger_id, idempotency_key) returns inserted=false). See
// db.RecordTriggerEvent.
func (s *Storage) RecordTriggerEvent(ctx context.Context, ev *models.TriggerEvent) (bool, error) {
	return s.db.RecordTriggerEvent(ctx, ev)
}

// SetTriggerEventRunID links an accepted event to the run it spawned.
func (s *Storage) SetTriggerEventRunID(ctx context.Context, eventID, runID uuid.UUID) error {
	return s.db.SetTriggerEventRunID(ctx, eventID, runID)
}

// GetTriggerEventByRunID returns the inbound event a run answers (for reply-back).
func (s *Storage) GetTriggerEventByRunID(ctx context.Context, runID uuid.UUID) (*models.TriggerEvent, error) {
	return s.db.GetTriggerEventByRunID(ctx, runID)
}

// connectorInheritance selects which of the template's write-capable connector
// facets a spawned event run inherits. A connector needs BOTH its MCP selection
// and its credential allowlist to actually write, so #511's opt-in gates both
// together; #177's webhook path predates the gate and inherits its historical
// subset (mcp only) unchanged.
type connectorInheritance struct {
	mcp  bool
	cred bool
}

// buildTriggerRun clones the trigger's template task into a fresh, immediately
// claimable one-shot run with the rendered prompt substituted. It deliberately
// drops Recurrence / ScheduledFor / TriggerType so the spawned run is a normal
// one-shot task that runs now rather than another inert trigger template. The
// `inherit` flags decide whether the run carries the template's write-capable
// connectors — the one place the event-trigger security default is enforced.
func (s *Storage) buildTriggerRun(ctx context.Context, taskID uuid.UUID, prompt string, inherit connectorInheritance) (uuid.UUID, error) {
	template, err := s.db.GetTask(ctx, taskID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("load template task: %w", err)
	}

	// An empty rendered prompt (no prompt_template configured) falls back to the
	// template task's own prompt, so a trigger can be a pure fire-the-task signal.
	if prompt == "" {
		prompt = template.Prompt
	}

	tc := models.TaskCreate{
		Prompt:                 prompt,
		Model:                  template.Model,
		FallbackModel:          template.FallbackModel,
		MaxIterations:          template.MaxIterations,
		Priority:               template.Priority,
		InstructionSelfImprove: template.InstructionSelfImprove,
		AllowNetwork:           template.AllowNetwork,
		AllowDelegation:        template.AllowDelegation,
		Files:                  template.Files,
		MaxRetries:             &template.MaxRetries,
		Timezone:               template.Timezone,
	}
	// Connector inheritance is the event-trigger security boundary: an untrusted
	// inbound event never carries the template's write-capable connectors unless
	// the template explicitly opted in (allow_event_triggers). Off ⇒ native tools
	// only (no MCP selection, no credentials).
	if inherit.mcp {
		tc.MCPSelection = template.MCPSelection
	}
	if inherit.cred {
		tc.CredentialAllowlist = template.CredentialAllowlist
	}

	run := models.NewTask(tc)
	run.CreatedBy = template.CreatedBy
	// Carry the originating API key forward so spawned-run cost keeps counting
	// against the template owner's spending caps.
	run.CreatedByKeyID = template.CreatedByKeyID

	if err := s.db.AddTask(ctx, run); err != nil {
		return uuid.Nil, fmt.Errorf("create trigger run: %w", err)
	}
	return run.ID, nil
}

// SpawnWebhookRun creates one fresh, immediately-claimable run cloned from the
// trigger's template task, with the rendered prompt substituted (#177). Behavior
// is unchanged: the spawned run inherits the template's MCP selection (its
// historical connector subset).
func (s *Storage) SpawnWebhookRun(ctx context.Context, trigger *models.TaskTrigger, prompt string) (uuid.UUID, error) {
	return s.buildTriggerRun(ctx, trigger.TaskID, prompt, connectorInheritance{mcp: true, cred: false})
}

// SpawnEmailRun creates one fresh run from an email trigger's template (#511).
// inheritConnectors reflects the template's allow_event_triggers opt-in: false
// (the secure default) spawns a native-tools-only run that carries NONE of the
// template's write-capable connectors, so an untrusted inbound email can never
// auto-escalate; true inherits both the MCP selection and its credential
// allowlist (a connector needs both to write).
func (s *Storage) SpawnEmailRun(ctx context.Context, trigger *models.TaskTrigger, prompt string, inheritConnectors bool) (uuid.UUID, error) {
	return s.buildTriggerRun(ctx, trigger.TaskID, prompt, connectorInheritance{mcp: inheritConnectors, cred: inheritConnectors})
}
