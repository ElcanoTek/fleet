package storage

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func countTasks(t *testing.T, store *Storage) int {
	t.Helper()
	_, total, err := store.GetTasksFiltered(db.TaskFilter{}, 1, 0)
	if err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	return total
}

// TestGetTriggerBySlug_ResolvesOnlyTheNamedKind pins the lookup side of the
// kind boundary, for every pair of kinds: a trigger is found only by the kind
// it has, a trigger of another kind is sql.ErrNoRows (the very answer an
// unknown slug gets, so a route cannot tell the two apart), and a kind that is
// not a trigger kind is an error rather than a match. A row whose stored kind
// is blank reads as webhook, as it does everywhere else.
func TestGetTriggerBySlug_ResolvesOnlyTheNamedKind(t *testing.T) {
	t.Parallel()

	store, database := newTestStore(t)
	ctx := context.Background()
	template := seedTemplateTask(t, store, nil, nil)

	for _, kind := range models.TriggerKinds {
		if err := store.CreateTrigger(ctx, &models.TaskTrigger{ID: uuid.New(), TaskID: template.ID, Slug: "s-" + string(kind), Secret: "s", Kind: kind}); err != nil {
			t.Fatalf("CreateTrigger %s: %v", kind, err)
		}
	}
	for _, stored := range models.TriggerKinds {
		for _, asked := range models.TriggerKinds {
			got, err := store.GetTriggerBySlug(ctx, asked, "s-"+string(stored))
			if stored == asked {
				if err != nil || got == nil || got.KindOrWebhook() != stored {
					t.Errorf("%q trigger looked up as %q: got %+v, err=%v; want the trigger", stored, asked, got, err)
				}
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) || got != nil {
				t.Errorf("%q trigger looked up as %q: got %+v, err=%v; want sql.ErrNoRows, as for an unknown slug", stored, asked, got, err)
			}
		}
	}
	if _, err := store.GetTriggerBySlug(ctx, models.TriggerKindWebhook, "no-such-slug"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("unknown slug: err=%v, want sql.ErrNoRows", err)
	}

	for _, bad := range []models.TriggerKind{"", "sms", "Email"} {
		got, err := store.GetTriggerBySlug(ctx, bad, "s-"+string(models.TriggerKindWebhook))
		if err == nil || errors.Is(err, sql.ErrNoRows) || got != nil {
			t.Errorf("lookup with kind %q: got %+v, err=%v; want an error naming the kind, not a row or a miss", bad, got, err)
		}
	}

	// A blank stored kind is a webhook trigger (the column default and
	// scanTrigger's reading), and is found as nothing else.
	if _, err := database.Conn().ExecContext(ctx, "UPDATE task_triggers SET kind = '' WHERE slug = $1", "s-"+string(models.TriggerKindWebhook)); err != nil {
		t.Fatalf("blank the kind: %v", err)
	}
	for _, asked := range models.TriggerKinds {
		got, err := store.GetTriggerBySlug(ctx, asked, "s-"+string(models.TriggerKindWebhook))
		if found := err == nil && got != nil; found != (asked == models.TriggerKindWebhook) {
			t.Errorf("blank-kind trigger looked up as %q: got %+v, err=%v", asked, got, err)
		}
	}
}

// TestSpawnTriggerRun_RefusesATriggerOfAnotherKind pins the spawn side of the
// kind boundary: each Spawn*Run applies the connector rule of ONE trigger
// kind, so it refuses a trigger of any other kind and creates no run. A
// handler that resolved a trigger without its kind (by ID, or by a lookup
// added later) therefore still cannot run an email trigger under the webhook
// rule, or the reverse. A kind added to models.TriggerKinds without a row
// here fails the completeness check at the end.
func TestSpawnTriggerRun_RefusesATriggerOfAnotherKind(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	ctx := context.Background()
	mcp := models.MCPSelection{{Server: "github"}}
	template := seedTemplateTask(t, store, mcp, nil)

	spawners := []struct {
		name  string
		kind  models.TriggerKind
		spawn func(*models.TaskTrigger) (uuid.UUID, error)
	}{
		{"SpawnWebhookRun", models.TriggerKindWebhook, func(tr *models.TaskTrigger) (uuid.UUID, error) {
			return store.SpawnWebhookRun(ctx, tr, "prompt")
		}},
		{"SpawnEmailRun (opted out)", models.TriggerKindEmail, func(tr *models.TaskTrigger) (uuid.UUID, error) {
			return store.SpawnEmailRun(ctx, tr, "prompt", false)
		}},
		{"SpawnEmailRun (opted in)", models.TriggerKindEmail, func(tr *models.TaskTrigger) (uuid.UUID, error) {
			return store.SpawnEmailRun(ctx, tr, "prompt", true)
		}},
	}

	covered := map[models.TriggerKind]bool{}
	for _, sp := range spawners {
		covered[sp.kind] = true
		for _, kind := range models.TriggerKinds {
			trig := &models.TaskTrigger{ID: uuid.New(), TaskID: template.ID, Slug: "k-" + string(kind), Kind: kind}
			before := countTasks(t, store)
			runID, err := sp.spawn(trig)
			after := countTasks(t, store)
			if kind == sp.kind {
				if err != nil || after != before+1 {
					t.Errorf("%s on a %q trigger: err=%v, %d new run(s); want one run", sp.name, kind, err, after-before)
				}
				continue
			}
			if err == nil || runID != uuid.Nil || after != before {
				t.Errorf("%s on a %q trigger: err=%v run=%s, %d new run(s); want a refusal and no run",
					sp.name, kind, err, runID, after-before)
			}
		}
	}
	for _, kind := range models.TriggerKinds {
		if !covered[kind] {
			t.Errorf("trigger kind %q has no Spawn*Run row in this test", kind)
		}
	}
}
