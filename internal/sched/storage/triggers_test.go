package storage

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func TestTriggerCRUDAndSpawn(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	// A webhook template task (inert) + a cron task to verify scoping.
	template := models.NewTask(models.TaskCreate{Prompt: "template prompt", TriggerType: models.TriggerTypeWebhook})
	if _, err := store.AddTask(template); err != nil {
		t.Fatalf("add template: %v", err)
	}
	if template.Status != models.TaskStatusScheduled {
		t.Errorf("webhook template status = %q, want scheduled (inert)", template.Status)
	}

	trig := &models.TaskTrigger{
		ID:             uuid.New(),
		TaskID:         template.ID,
		Slug:           "deploy-hook",
		Secret:         "secret-value-at-least-32-bytes-long!",
		PromptTemplate: "Deploy: {{.Payload}}",
	}
	if err := store.CreateTrigger(ctx, trig); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	// GetTriggerBySlug round-trips.
	got, err := store.GetTriggerBySlug(ctx, "deploy-hook")
	if err != nil {
		t.Fatalf("get by slug: %v", err)
	}
	if got.ID != trig.ID || got.Secret != trig.Secret || got.PromptTemplate != trig.PromptTemplate {
		t.Errorf("round-trip mismatch: got %+v", got)
	}

	// GetTrigger by ID.
	if _, err := store.GetTrigger(ctx, trig.ID); err != nil {
		t.Fatalf("get by id: %v", err)
	}

	// ListTriggers scoped to the task.
	list, err := store.ListTriggers(ctx, &template.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list scoped: got %d, want 1", len(list))
	}

	// Rotate invalidates the old secret.
	ok, err := store.RotateTriggerSecret(ctx, trig.ID, "rotated-secret-also-32-bytes-long!!")
	if err != nil || !ok {
		t.Fatalf("rotate: ok=%v err=%v", ok, err)
	}
	rotated, _ := store.GetTrigger(ctx, trig.ID)
	if rotated.Secret == trig.Secret {
		t.Error("rotate did not change the secret")
	}

	// SpawnWebhookRun clones the template into a fresh claimable run.
	runID, err := store.SpawnWebhookRun(ctx, trig, "rendered prompt")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	run, err := store.GetTask(runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if run.Status != models.TaskStatusPending {
		t.Errorf("run status = %q, want pending", run.Status)
	}
	if run.TriggerType != models.TriggerTypeCron {
		t.Errorf("run trigger_type = %q, want cron", run.TriggerType)
	}
	if run.Prompt != "rendered prompt" {
		t.Errorf("run prompt = %q", run.Prompt)
	}

	// Empty rendered prompt falls back to the template task's own prompt.
	fallbackID, err := store.SpawnWebhookRun(ctx, trig, "")
	if err != nil {
		t.Fatalf("spawn fallback: %v", err)
	}
	fallback, _ := store.GetTask(fallbackID)
	if fallback.Prompt != "template prompt" {
		t.Errorf("fallback prompt = %q, want template prompt", fallback.Prompt)
	}

	// Delete removes it; ON DELETE CASCADE also fires when the task is deleted.
	deleted, err := store.DeleteTrigger(ctx, trig.ID)
	if err != nil || !deleted {
		t.Fatalf("delete: deleted=%v err=%v", deleted, err)
	}
	if _, err := store.GetTriggerBySlug(ctx, "deploy-hook"); err == nil {
		t.Error("expected error after delete")
	}
}
