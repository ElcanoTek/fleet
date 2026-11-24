package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func TestTriggerCRUDAndSpawn(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	ctx := context.Background()

	// A webhook template task (inert) + a cron task to verify scoping.
	maxRetries := 2
	template := models.NewTask(models.TaskCreate{
		Prompt:       "template prompt",
		TriggerType:  models.TriggerTypeWebhook,
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`),
		MaxRetries:   &maxRetries,
		RetryPolicy:  &models.RetryPolicy{RetryOn: []string{models.FailureOutputFormat}},
	})
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
	got, err := store.GetTriggerBySlug(ctx, models.TriggerKindWebhook, "deploy-hook")
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
	if len(run.OutputSchema) == 0 {
		t.Error("trigger run lost the template's structured-output contract")
	}
	if run.MaxRetries != 2 || run.RetryPolicy == nil || !run.RetryPolicy.ShouldRetryClass(models.FailureOutputFormat) {
		t.Errorf("trigger run lost structured-output retry policy: retries=%d policy=%+v", run.MaxRetries, run.RetryPolicy)
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
	if _, err := store.GetTriggerBySlug(ctx, models.TriggerKindWebhook, "deploy-hook"); err == nil {
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
	t.Parallel()

	store, _ := newTestStore(t)
	task := seedTemplateTask(t, store, nil, nil)
	ctx := context.Background()

	// Email trigger with a policy round-trips through the DB.
	pol := &models.EmailTriggerPolicy{ApprovedSenders: []string{"corp.com"}, RequireDKIM: true, RequireSPF: true, MaxAttachments: 2, MaxAttachmentBytes: 4096}
	if err := store.CreateTrigger(ctx, &models.TaskTrigger{ID: uuid.New(), TaskID: task.ID, Slug: "e1", Secret: "s", Kind: models.TriggerKindEmail, EmailPolicy: pol}); err != nil {
		t.Fatalf("CreateTrigger email: %v", err)
	}
	got, err := store.GetTriggerBySlug(ctx, models.TriggerKindEmail, "e1")
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
	wh, err := store.GetTriggerBySlug(ctx, models.TriggerKindWebhook, "w1")
	if err != nil {
		t.Fatalf("GetTriggerBySlug webhook: %v", err)
	}
	if wh.KindOrWebhook() != models.TriggerKindWebhook || wh.EmailPolicy != nil {
		t.Errorf("webhook trigger wrong: kind=%q policy=%+v", wh.Kind, wh.EmailPolicy)
	}
}

// seedEmailTrigger stores an email trigger on template, for the spawns that
// record their trigger event against it.
func seedEmailTrigger(t *testing.T, store *Storage, template *models.Task, slug string) *models.TaskTrigger {
	t.Helper()
	trig := &models.TaskTrigger{ID: uuid.New(), TaskID: template.ID, Slug: slug, Secret: "s", Kind: models.TriggerKindEmail, EmailPolicy: &models.EmailTriggerPolicy{}}
	if err := store.CreateTrigger(context.Background(), trig); err != nil {
		t.Fatalf("CreateTrigger: %v", err)
	}
	return trig
}

func TestSpawnEmailRun_Dedup(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	trig := seedEmailTrigger(t, store, seedTemplateTask(t, store, nil, nil), "d1")
	ctx := context.Background()

	firstID, dup, err := store.SpawnEmailRun(ctx, trig, &models.TriggerEvent{IdempotencyKey: "<m1>", Sender: "a@corp.com"}, "prompt", false)
	if err != nil || dup || firstID == uuid.Nil {
		t.Fatalf("first delivery: run=%s duplicate=%v err=%v (want a run)", firstID, dup, err)
	}
	// Same key again → a duplicate, and no second run.
	before := countTasks(t, store)
	againID, dup, err := store.SpawnEmailRun(ctx, trig, &models.TriggerEvent{IdempotencyKey: "<m1>", Sender: "a@corp.com"}, "prompt", false)
	if err != nil || !dup || againID != uuid.Nil {
		t.Fatalf("duplicate delivery: run=%s duplicate=%v err=%v (want duplicate, no run)", againID, dup, err)
	}
	if after := countTasks(t, store); after != before {
		t.Fatalf("duplicate delivery created %d run(s)", after-before)
	}
	// A different key → a new run.
	otherID, dup, err := store.SpawnEmailRun(ctx, trig, &models.TriggerEvent{IdempotencyKey: "<m2>", Sender: "a@corp.com"}, "prompt", false)
	if err != nil || dup || otherID == uuid.Nil || otherID == firstID {
		t.Fatalf("second key: run=%s duplicate=%v err=%v (want a new run)", otherID, dup, err)
	}
}

// TestSpawnEmailRun_FailedRunInsertRecordsNothing: the trigger event and the
// run commit together, so a run insert that fails leaves no event behind, and
// the same delivery again is not a duplicate but starts the run.
func TestSpawnEmailRun_FailedRunInsertRecordsNothing(t *testing.T) {
	t.Parallel()

	store, database := newTestStore(t)
	trig := seedEmailTrigger(t, store, seedTemplateTask(t, store, nil, nil), "f1")
	ctx := context.Background()
	conn := database.Conn()

	// NOT VALID: the template row is not checked, only the run insert.
	if _, err := conn.ExecContext(ctx, `ALTER TABLE sched.tasks ADD CONSTRAINT refuse_runs CHECK (prompt <> 'refused prompt') NOT VALID`); err != nil {
		t.Fatalf("add constraint: %v", err)
	}
	runID, dup, err := store.SpawnEmailRun(ctx, trig, &models.TriggerEvent{IdempotencyKey: "<f1>"}, "refused prompt", false)
	if err == nil || dup || runID != uuid.Nil {
		t.Fatalf("spawn with a failing run insert: run=%s duplicate=%v err=%v (want an error)", runID, dup, err)
	}
	var events int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM sched.trigger_events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 0 {
		t.Fatalf("a spawn that created no run left %d trigger event(s)", events)
	}

	if _, err := conn.ExecContext(ctx, `ALTER TABLE sched.tasks DROP CONSTRAINT refuse_runs`); err != nil {
		t.Fatalf("drop constraint: %v", err)
	}
	runID, dup, err = store.SpawnEmailRun(ctx, trig, &models.TriggerEvent{IdempotencyKey: "<f1>"}, "refused prompt", false)
	if err != nil || dup || runID == uuid.Nil {
		t.Fatalf("the same delivery again: run=%s duplicate=%v err=%v (want a run)", runID, dup, err)
	}
}

func TestTriggerEvent_RunLinkage(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	trig := seedEmailTrigger(t, store, seedTemplateTask(t, store, nil, nil), "l1")
	ctx := context.Background()
	ev := &models.TriggerEvent{IdempotencyKey: "<link>", Sender: "a@corp.com", Subject: "hi", MessageID: "<link>"}
	runID, _, err := store.SpawnEmailRun(ctx, trig, ev, "prompt", false)
	if err != nil {
		t.Fatalf("SpawnEmailRun: %v", err)
	}
	got, err := store.GetTriggerEventByRunID(ctx, runID)
	if err != nil {
		t.Fatalf("GetTriggerEventByRunID: %v", err)
	}
	if got.RunID == nil || *got.RunID != runID || got.ID != ev.ID || got.TriggerID != trig.ID || got.Sender != "a@corp.com" || got.MessageID != "<link>" {
		t.Errorf("linked event wrong: %+v (spawned with %+v)", got, ev)
	}
	// The same event by its key, and no event for a key never recorded.
	byKey, err := store.GetTriggerEventByKey(ctx, trig.ID, "<link>")
	if err != nil || byKey.ID != ev.ID || byKey.RunID == nil || *byKey.RunID != runID {
		t.Errorf("GetTriggerEventByKey = %+v, %v; want the linked event", byKey, err)
	}
	if _, err := store.GetTriggerEventByKey(ctx, trig.ID, "<never>"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("GetTriggerEventByKey for an unrecorded key: err = %v, want sql.ErrNoRows", err)
	}
}

func TestSpawnEmailRun_ConnectorGating(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	mcp := models.MCPSelection{{Server: "github"}}
	cred := models.CredentialAllowlist{{Server: "github"}}
	task := seedTemplateTask(t, store, mcp, cred)
	trig := seedEmailTrigger(t, store, task, "g1")
	ctx := context.Background()

	// Opt-out: NO connectors (neither MCP selection nor credential allowlist).
	outID, _, err := store.SpawnEmailRun(ctx, trig, &models.TriggerEvent{IdempotencyKey: "<out>"}, "prompt", false)
	if err != nil {
		t.Fatalf("SpawnEmailRun opt-out: %v", err)
	}
	out, _ := store.GetTask(outID)
	if len(out.MCPSelection) != 0 || len(out.CredentialAllowlist) != 0 {
		t.Errorf("opt-out run inherited connectors: mcp=%+v cred=%+v", out.MCPSelection, out.CredentialAllowlist)
	}
	// ...and the deny is EXPLICIT, not merely absent (#979). A nil allowlist is
	// "inherit global" — every seat — so the opted-OUT run must persist a non-nil
	// empty list, which survives the nullable JSONB round trip as "deny all".
	// Asserting only len()==0 above is what let the boundary invert unnoticed.
	if out.CredentialAllowlist == nil {
		t.Error("opt-out run has a NIL credential allowlist: nil means inherit-global (ALL seats), not none")
	}

	// Opt-in: inherits BOTH.
	inID, _, err := store.SpawnEmailRun(ctx, trig, &models.TriggerEvent{IdempotencyKey: "<in>"}, "prompt", true)
	if err != nil {
		t.Fatalf("SpawnEmailRun opt-in: %v", err)
	}
	in, _ := store.GetTask(inID)
	if len(in.MCPSelection) != 1 || len(in.CredentialAllowlist) != 1 {
		t.Errorf("opt-in run should inherit both: mcp=%+v cred=%+v", in.MCPSelection, in.CredentialAllowlist)
	}
}

func TestSpawnWebhookRun_PreservesConnectorBehavior(t *testing.T) {
	t.Parallel()

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

// TestSpawnTriggerRun_CarriesRunIfGate pins the RunIf enforcement contract on
// the trigger-spawn path: a webhook/email trigger whose template carries a
// pre-run gate must spawn runs that carry the SAME gate and land on the
// scheduler's promotion path (scheduled, scheduled_for set), so the gate is
// evaluated before dispatch. Before the fix buildTriggerRun dropped run_if and
// minted the run pending — firing a trigger executed the gated work with the
// admin-authored condition silently discarded.
func TestSpawnTriggerRun_CarriesRunIfGate(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	template := seedTemplateTask(t, store, nil, nil)
	gate := &models.RunIf{Command: "test -f /tmp/ready", ExitCodeIs: 0, TimeoutSeconds: 30}
	template.RunIf = gate
	if _, err := store.UpdateTask(template); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	trig := &models.TaskTrigger{TaskID: template.ID}

	runID, err := store.SpawnWebhookRun(context.Background(), trig, "prompt")
	if err != nil {
		t.Fatalf("SpawnWebhookRun: %v", err)
	}
	run, err := store.GetTask(runID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if run.RunIf == nil || run.RunIf.Command != gate.Command {
		t.Fatalf("spawned run must carry the template's run_if, got %+v", run.RunIf)
	}
	if run.Status != models.TaskStatusScheduled {
		t.Errorf("gated spawned run status = %q, want scheduled (pending would dispatch with the gate unevaluated)", run.Status)
	}
	if run.ScheduledFor == nil {
		t.Error("gated spawned run must have scheduled_for set so the scheduler evaluates its gate")
	}
}
