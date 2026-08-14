package storage

import (
	"context"
	"encoding/json"
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
//
// It is a closed three-way enum rather than a pair of booleans because "did not
// inherit" is NOT the same statement as "may reach nothing", and conflating the
// two is exactly how #979 happened: leaving the credential allowlist unset on
// the opted-OUT spawn produced a nil allowlist, and nil means "inherit global"
// — every seat — at every consumer (agentcore.CredentialAllowlist.Permits).
// The secure default therefore has to SET something, not omit it.
type connectorInheritance int

const (
	// connectorsDenied is the secure default for event ingress (#511): the
	// spawned run carries none of the template's connectors and may call no MCP
	// server at all. Encoded EXPLICITLY as a non-nil empty credential allowlist
	// (deny all) — see the type comment for why absent is not an option.
	connectorsDenied connectorInheritance = iota
	// connectorsMCPOnly is #177's historical webhook subset, unchanged: the run
	// inherits the template's MCP selection and its credential allowlist is left
	// to the deployment default (nil ⇒ inherit global). Webhook ingress is
	// HMAC-authenticated against a per-trigger secret, so its trust model is the
	// operator who holds that secret, not an arbitrary inbound sender.
	connectorsMCPOnly
	// connectorsInherited is the allow_event_triggers=true opt-in (#511): the run
	// carries BOTH facets exactly as the template declares them, including a nil
	// credential allowlist when the template itself never scoped one.
	connectorsInherited
)

// buildTriggerRun clones the trigger's template task into a fresh one-shot run
// with the rendered prompt substituted. It deliberately drops Recurrence /
// ScheduledFor / TriggerType so the spawned run is a normal one-shot task that
// runs now rather than another inert trigger template — immediately claimable
// for an ungated template; parked scheduled-for-now when the template carries
// a run_if gate, so the scheduler evaluates the gate before dispatch. The
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
		AllowDelegation:        models.BoolPtr(template.AllowDelegation),
		ThinkingBudgetTokens:   template.ThinkingBudgetTokens,
		OutputSchema:           append(json.RawMessage(nil), template.OutputSchema...),
		Files:                  template.Files,
		FileNames:              template.FileNames,
		MaxRetries:             &template.MaxRetries,
		RetryPolicy:            template.RetryPolicy,
		Timezone:               template.Timezone,
		// The pre-run gate is part of the definition every spawned run must
		// honor (models.RunIf's enforcement contract): NewTask parks the gated
		// run scheduled-for-now, so the scheduler evaluates the gate before
		// promoting it. Without this carry, firing a trigger executed the
		// template's gated work with the admin-authored condition silently
		// dropped.
		RunIf: template.RunIf,
	}
	// Connector inheritance is the event-trigger security boundary: an untrusted
	// inbound event never carries the template's write-capable connectors unless
	// the template explicitly opted in (allow_event_triggers). Off ⇒ native tools
	// only (no MCP selection, no credentials).
	switch inherit {
	case connectorsInherited:
		tc.MCPSelection = template.MCPSelection
		tc.CredentialAllowlist = template.CredentialAllowlist
	case connectorsMCPOnly:
		tc.MCPSelection = template.MCPSelection
	default: // connectorsDenied
		// Deny-all, stated explicitly. The credential allowlist is the load-bearing
		// half: it is a NULLABLE column that round-trips nil-vs-empty (#184), so an
		// empty list persists as "deny all" and the scheduled runner refuses to wire
		// ANY connector for the run — local bundle servers and the owner's hosted
		// remote connections alike. The MCP selection is deliberately left nil: the
		// mcp_selection column is coerced to `[]` on write (db.mcpSelectionOrEmpty),
		// so an "explicitly empty" selection is indistinguishable from an absent one
		// and cannot carry the deny by itself. See docs/EVENT-TRIGGERS.md.
		tc.CredentialAllowlist = models.CredentialAllowlist{}
	}

	run := models.NewTask(tc)
	run.CreatedBy = template.CreatedBy
	// Carry the originating API key forward so spawned-run cost keeps counting
	// against the template owner's spending caps.
	run.CreatedByKeyID = template.CreatedByKeyID

	// Route every spawned run through the same schema/contract gate as public
	// enqueue paths. Event triggers must not create a task the API would reject.
	if _, err := s.AddTaskWithContext(ctx, run); err != nil {
		return uuid.Nil, fmt.Errorf("create trigger run: %w", err)
	}
	return run.ID, nil
}

// SpawnWebhookRun creates one fresh run cloned from the trigger's template
// task, with the rendered prompt substituted (#177). Behavior is unchanged:
// the spawned run inherits the template's MCP selection (its historical
// connector subset).
func (s *Storage) SpawnWebhookRun(ctx context.Context, trigger *models.TaskTrigger, prompt string) (uuid.UUID, error) {
	return s.buildTriggerRun(ctx, trigger.TaskID, prompt, connectorsMCPOnly)
}

// SpawnEmailRun creates one fresh run from an email trigger's template (#511).
// inheritConnectors reflects the template's allow_event_triggers opt-in: false
// (the secure default) spawns a native-tools-only run whose credential allowlist
// is an explicit deny-all, so an untrusted inbound email can never auto-escalate
// through a connector; true inherits both the MCP selection and its credential
// allowlist (a connector needs both to write).
func (s *Storage) SpawnEmailRun(ctx context.Context, trigger *models.TaskTrigger, prompt string, inheritConnectors bool) (uuid.UUID, error) {
	inherit := connectorsDenied
	if inheritConnectors {
		inherit = connectorsInherited
	}
	return s.buildTriggerRun(ctx, trigger.TaskID, prompt, inherit)
}
