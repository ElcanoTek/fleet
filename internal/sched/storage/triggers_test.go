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

func seedTemplateTask(t *testing.T, store *Storage, mcp models.MCPSelection, cred models.CredentialAllowlist) *models.Task {
	t.Helper()
	task := &models.Task{
		ID:                  uuid.New(),
		Prompt:              "template",
		Status:              models.TaskStatusScheduled,
		Priority:            models.PriorityNormal,
		TriggerType:         models.TriggerTypeWebhook,
		MCPSelection:        mcp,
		CredentialAllowlist: cred,
	}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	return task
}

func TestCreateTrigger_KindPolicyRoundTrip(t *testing.T) {
	store, _ := newTestStore(t)
	task := seedTemplateTask(t, store, nil, nil)
	ctx := context.Background()

	// Email trigger with a policy round-trips through the DB.
	pol := &models.EmailTriggerPolicy{ApprovedSenders: []string{"corp.com"}, RequireDKIM: true, RequireSPF: true, MaxAttachments: 2, MaxAttachmentBytes: 4096}
	if err := store.CreateTrigger(ctx, &models.TaskTrigger{ID: uuid.New(), TaskID: task.ID, Slug: "e1", Secret: "s", Kind: models.TriggerKindEmail, EmailPolicy: pol}); err != nil {
		t.Fatalf("CreateTrigger email: %v", err)
	}
	got, err := store.GetTriggerBySlug(ctx, "e1")
	if err != nil {
		t.Fatalf("GetTriggerBySlug: %v", err)
	}
	if got.KindOrWebhook() != models.TriggerKindEmail {
		t.Errorf("kind = %q, want email", got.Kind)
	}
	if got.EmailPolicy == nil || len(got.EmailPolicy.ApprovedSenders) != 1 || !got.EmailPolicy.RequireDKIM ||
		!got.EmailPolicy.RequireSPF || got.EmailPolicy.MaxAttachments != 2 || got.EmailPolicy.MaxAttachmentBytes != 4096 {
		t.Errorf("email policy lost on round trip: %+v", got.EmailPolicy)
	}

	// A webhook trigger (no kind set) defaults to webhook and has no policy.
	if err := store.CreateTrigger(ctx, &models.TaskTrigger{ID: uuid.New(), TaskID: task.ID, Slug: "w1", Secret: "s"}); err != nil {
		t.Fatalf("CreateTrigger webhook: %v", err)
	}
	wh, err := store.GetTriggerBySlug(ctx, "w1")
	if err != nil {
		t.Fatalf("GetTriggerBySlug webhook: %v", err)
	}
	if wh.KindOrWebhook() != models.TriggerKindWebhook || wh.EmailPolicy != nil {
		t.Errorf("webhook trigger wrong: kind=%q policy=%+v", wh.Kind, wh.EmailPolicy)
	}
}

func TestRecordTriggerEvent_Dedup(t *testing.T) {
	store, _ := newTestStore(t)
	task := seedTemplateTask(t, store, nil, nil)
	ctx := context.Background()
	trigID := uuid.New()
	if err := store.CreateTrigger(ctx, &models.TaskTrigger{ID: trigID, TaskID: task.ID, Slug: "d1", Secret: "s", Kind: models.TriggerKindEmail, EmailPolicy: &models.EmailTriggerPolicy{}}); err != nil {
		t.Fatalf("CreateTrigger: %v", err)
	}

	first, err := store.RecordTriggerEvent(ctx, &models.TriggerEvent{TriggerID: trigID, IdempotencyKey: "<m1>", Sender: "a@corp.com"})
	if err != nil || !first {
		t.Fatalf("first record: inserted=%v err=%v (want true,nil)", first, err)
	}
	// Same key again → NOT inserted (dedup).
	dup, err := store.RecordTriggerEvent(ctx, &models.TriggerEvent{TriggerID: trigID, IdempotencyKey: "<m1>", Sender: "a@corp.com"})
	if err != nil || dup {
		t.Fatalf("duplicate record: inserted=%v err=%v (want false,nil)", dup, err)
	}
	// A different key → inserted.
	other, err := store.RecordTriggerEvent(ctx, &models.TriggerEvent{TriggerID: trigID, IdempotencyKey: "<m2>", Sender: "a@corp.com"})
	if err != nil || !other {
		t.Fatalf("second key: inserted=%v err=%v (want true,nil)", other, err)
	}
}

func TestTriggerEvent_RunLinkage(t *testing.T) {
	store, _ := newTestStore(t)
	task := seedTemplateTask(t, store, nil, nil)
	ctx := context.Background()
	trigID := uuid.New()
	if err := store.CreateTrigger(ctx, &models.TaskTrigger{ID: trigID, TaskID: task.ID, Slug: "l1", Secret: "s", Kind: models.TriggerKindEmail, EmailPolicy: &models.EmailTriggerPolicy{}}); err != nil {
		t.Fatalf("CreateTrigger: %v", err)
	}
	ev := &models.TriggerEvent{TriggerID: trigID, IdempotencyKey: "<link>", Sender: "a@corp.com", Subject: "hi", MessageID: "<link>"}
	if _, err := store.RecordTriggerEvent(ctx, ev); err != nil {
		t.Fatalf("record: %v", err)
	}
	runID := uuid.New()
	if err := store.SetTriggerEventRunID(ctx, ev.ID, runID); err != nil {
		t.Fatalf("SetTriggerEventRunID: %v", err)
	}
	got, err := store.GetTriggerEventByRunID(ctx, runID)
	if err != nil {
		t.Fatalf("GetTriggerEventByRunID: %v", err)
	}
	if got.RunID == nil || *got.RunID != runID || got.Sender != "a@corp.com" || got.MessageID != "<link>" {
		t.Errorf("linked event wrong: %+v", got)
	}
}

func TestSpawnEmailRun_ConnectorGating(t *testing.T) {
	store, _ := newTestStore(t)
	mcp := models.MCPSelection{{Server: "github"}}
	cred := models.CredentialAllowlist{{Server: "github"}}
	task := seedTemplateTask(t, store, mcp, cred)
	trig := &models.TaskTrigger{TaskID: task.ID}
	ctx := context.Background()

	// Opt-out: NO connectors (neither MCP selection nor credential allowlist).
	outID, err := store.SpawnEmailRun(ctx, trig, "prompt", false)
	if err != nil {
		t.Fatalf("SpawnEmailRun opt-out: %v", err)
	}
	out, _ := store.GetTask(outID)
	if len(out.MCPSelection) != 0 || len(out.CredentialAllowlist) != 0 {
		t.Errorf("opt-out run inherited connectors: mcp=%+v cred=%+v", out.MCPSelection, out.CredentialAllowlist)
	}

	// Opt-in: inherits BOTH.
	inID, err := store.SpawnEmailRun(ctx, trig, "prompt", true)
	if err != nil {
		t.Fatalf("SpawnEmailRun opt-in: %v", err)
	}
	in, _ := store.GetTask(inID)
	if len(in.MCPSelection) != 1 || len(in.CredentialAllowlist) != 1 {
		t.Errorf("opt-in run should inherit both: mcp=%+v cred=%+v", in.MCPSelection, in.CredentialAllowlist)
	}
}

func TestSpawnWebhookRun_PreservesConnectorBehavior(t *testing.T) {
	store, _ := newTestStore(t)
	mcp := models.MCPSelection{{Server: "github"}}
	cred := models.CredentialAllowlist{{Server: "github"}}
	task := seedTemplateTask(t, store, mcp, cred)
	trig := &models.TaskTrigger{TaskID: task.ID}

	runID, err := store.SpawnWebhookRun(context.Background(), trig, "prompt")
	if err != nil {
		t.Fatalf("SpawnWebhookRun: %v", err)
	}
	run, _ := store.GetTask(runID)
	// #177 behavior preserved: inherits the MCP selection but NOT the credential
	// allowlist (unchanged by #511).
	if len(run.MCPSelection) != 1 {
		t.Errorf("webhook run should inherit MCP selection, got %+v", run.MCPSelection)
	}
	if len(run.CredentialAllowlist) != 0 {
		t.Errorf("webhook run should NOT inherit credential allowlist, got %+v", run.CredentialAllowlist)
	}
}
