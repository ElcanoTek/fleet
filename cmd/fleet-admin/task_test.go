package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// fakeDefinitionStore is an in-memory definitionStore for DB-free tests of the
// definition export/import CLI seam.
type fakeDefinitionStore struct {
	tasks   []*models.Task
	byName  map[string]*models.Task
	added   []*models.Task
	updated []*models.Task
}

func newFakeDefinitionStore(tasks ...*models.Task) *fakeDefinitionStore {
	f := &fakeDefinitionStore{byName: map[string]*models.Task{}}
	for _, t := range tasks {
		f.tasks = append(f.tasks, t)
		if t.Name != "" {
			f.byName[t.Name] = t
		}
	}
	return f
}

func (f *fakeDefinitionStore) ListTasksForExport(_ context.Context, _ []uuid.UUID, _ bool) ([]*models.Task, error) {
	return f.tasks, nil
}
func (f *fakeDefinitionStore) FindTaskIDsByName(_ context.Context, names []string) (map[string]uuid.UUID, error) {
	out := map[string]uuid.UUID{}
	for _, n := range names {
		if t, ok := f.byName[n]; ok {
			out[n] = t.ID
		}
	}
	return out, nil
}
func (f *fakeDefinitionStore) GetTaskByName(_ context.Context, name string) (*models.Task, error) {
	if t, ok := f.byName[name]; ok {
		return t, nil
	}
	return nil, nil
}
func (f *fakeDefinitionStore) AddTaskWithContext(_ context.Context, t *models.Task) (*models.Task, error) {
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	f.added = append(f.added, t)
	f.tasks = append(f.tasks, t)
	if t.Name != "" {
		f.byName[t.Name] = t
	}
	return t, nil
}
func (f *fakeDefinitionStore) UpdateTask(t *models.Task) (*models.Task, error) {
	f.updated = append(f.updated, t)
	if t.Name != "" {
		f.byName[t.Name] = t
	}
	for i, ex := range f.tasks {
		if ex.ID == t.ID {
			f.tasks[i] = t
		}
	}
	return t, nil
}

func TestExportTaskDefinitions_JSON(t *testing.T) {
	orig := &models.Task{ID: uuid.New(), Name: "daily", Prompt: "do it", Recurrence: "0 9 * * *", Priority: 3}
	st := newFakeDefinitionStore(orig)
	var buf bytes.Buffer
	n, err := exportTaskDefinitions(context.Background(), st, &buf, "json", nil, false)
	if err != nil || n != 1 {
		t.Fatalf("export n=%d err=%v", n, err)
	}
	var env models.TaskExportEnvelope
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Version != models.TaskExportVersion || len(env.Tasks) != 1 {
		t.Fatalf("envelope = %+v", env)
	}
	if env.Tasks[0].Name != "daily" || env.Tasks[0].Recurrence != "0 9 * * *" {
		t.Errorf("record = %+v", env.Tasks[0])
	}
}

func TestExportTaskDefinitions_YAML(t *testing.T) {
	orig := &models.Task{ID: uuid.New(), Name: "weekly", Prompt: "report", Recurrence: "0 9 * * MON"}
	st := newFakeDefinitionStore(orig)
	var buf bytes.Buffer
	if _, err := exportTaskDefinitions(context.Background(), st, &buf, "yaml", nil, false); err != nil {
		t.Fatalf("export yaml: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("name: weekly")) {
		t.Errorf("yaml missing name: %s", buf.String())
	}
}

func TestImportTaskDefinitions_CreateAndConflict(t *testing.T) {
	env := models.TaskExportEnvelope{
		Version: models.TaskExportVersion,
		Tasks: []models.TaskExportRecord{
			{Name: "alpha", Prompt: "a", Recurrence: "0 9 * * *"},
			{Name: "beta", Prompt: "b", Recurrence: "0 10 * * *"},
		},
	}
	body, _ := json.Marshal(env)

	// Fresh store → both created.
	st := newFakeDefinitionStore()
	resp, err := importTaskDefinitions(context.Background(), st, bytes.NewReader(body), "", false, models.TaskImportConflictError)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if resp.Created != 2 || len(st.added) != 2 {
		t.Fatalf("resp = %+v, added = %d", resp, len(st.added))
	}

	// conflict=error → error when a name collides.
	st2 := newFakeDefinitionStore(&models.Task{ID: uuid.New(), Name: "alpha", Prompt: "old", Recurrence: "0 9 * * *"})
	if _, err := importTaskDefinitions(context.Background(), st2, bytes.NewReader(body), "", false, models.TaskImportConflictError); err == nil {
		t.Fatal("conflict=error should abort on collision")
	}

	// conflict=skip → alpha skipped, beta created.
	st3 := newFakeDefinitionStore(&models.Task{ID: uuid.New(), Name: "alpha", Prompt: "old", Recurrence: "0 9 * * *"})
	resp, err = importTaskDefinitions(context.Background(), st3, bytes.NewReader(body), "", false, models.TaskImportConflictSkip)
	if err != nil {
		t.Fatalf("skip: %v", err)
	}
	if resp.Created != 1 || resp.Skipped != 1 {
		t.Fatalf("skip resp = %+v", resp)
	}
	if st3.byName["alpha"].Prompt != "old" {
		t.Error("skip must not mutate alpha")
	}

	// conflict=replace → alpha replaced in place, beta created.
	st4 := newFakeDefinitionStore(&models.Task{ID: uuid.New(), Name: "alpha", Prompt: "old", Recurrence: "0 9 * * *", Status: models.TaskStatusScheduled, AttemptCount: 7})
	resp, err = importTaskDefinitions(context.Background(), st4, bytes.NewReader(body), "", false, models.TaskImportConflictReplace)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if resp.Replaced != 1 || resp.Created != 1 {
		t.Fatalf("replace resp = %+v", resp)
	}
	alpha := st4.byName["alpha"]
	if alpha.Prompt != "a" {
		t.Errorf("replace did not update alpha: %+v", alpha)
	}
	if alpha.AttemptCount != 7 || alpha.Status != models.TaskStatusScheduled {
		t.Errorf("replace clobbered runtime state: status=%s attempt=%d", alpha.Status, alpha.AttemptCount)
	}

	// dry_run → no writes, plan returned.
	st5 := newFakeDefinitionStore()
	resp, err = importTaskDefinitions(context.Background(), st5, bytes.NewReader(body), "", true, models.TaskImportConflictError)
	if err != nil {
		t.Fatalf("dry_run: %v", err)
	}
	if !resp.DryRun || resp.Created != 2 || len(st5.added) != 0 {
		t.Fatalf("dry_run resp = %+v, added = %d", resp, len(st5.added))
	}
}

func TestImportTaskDefinitions_RejectsUnknownVersion(t *testing.T) {
	st := newFakeDefinitionStore()
	body := []byte(`{"version":"99","tasks":[]}`)
	if _, err := importTaskDefinitions(context.Background(), st, bytes.NewReader(body), "", false, models.TaskImportConflictError); err == nil {
		t.Fatal("expected unknown-version error")
	}
}

func TestImportTaskDefinitions_RejectsIntraBatchDupe(t *testing.T) {
	st := newFakeDefinitionStore()
	env := models.TaskExportEnvelope{Version: models.TaskExportVersion, Tasks: []models.TaskExportRecord{
		{Name: "dup", Prompt: "a", Recurrence: "0 9 * * *"},
		{Name: "dup", Prompt: "b", Recurrence: "0 10 * * *"},
	}}
	body, _ := json.Marshal(env)
	if _, err := importTaskDefinitions(context.Background(), st, bytes.NewReader(body), "", false, models.TaskImportConflictSkip); err == nil {
		t.Fatal("expected intra-batch dupe error")
	}
}

// TestExportImportDefinitions_DB is the end-to-end proof against the real sched
// DB (gated on DATABASE_URL — the sched-suite convention; skips when absent).
func TestExportImportDefinitions_DB(t *testing.T) {
	database := db.New()
	if err := database.Init("", db.DefaultPoolConfig()); err != nil {
		t.Skipf("sched DB unavailable: %v", err)
	}
	ctx := context.Background()
	conn, err := database.Conn().Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock(2)"); err != nil {
		conn.Close()
		t.Fatalf("lock: %v", err)
	}
	clean := func() {
		for _, q := range []string{"DELETE FROM logs", "DELETE FROM tasks", "DELETE FROM users"} {
			database.Conn().ExecContext(ctx, q)
		}
	}
	clean()
	t.Cleanup(func() {
		clean()
		conn.ExecContext(ctx, "SELECT pg_advisory_unlock(2)")
		conn.Close()
		database.Close()
	})

	st := storage.New()
	st.SetDatabase(database)

	// Seed a named recurring task.
	future := time.Now().Add(48 * time.Hour).UTC()
	seed := models.NewTask(models.TaskCreate{Name: "db-roundtrip", Prompt: "db def round-trip", Recurrence: "0 8 * * *", ScheduledFor: &future})
	seed.Status = models.TaskStatusScheduled
	if _, err := st.AddTaskWithContext(ctx, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Export to JSON.
	var buf bytes.Buffer
	if _, err := exportTaskDefinitions(ctx, st, &buf, "json", nil, false); err != nil {
		t.Fatalf("export: %v", err)
	}

	// Wipe, then import the export back.
	clean()
	resp, err := importTaskDefinitions(ctx, st, bytes.NewReader(buf.Bytes()), "", false, models.TaskImportConflictError)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if resp.Created != 1 {
		t.Fatalf("import created = %d, want 1", resp.Created)
	}
	got, err := st.GetTaskByName(ctx, "db-roundtrip")
	if err != nil || got == nil {
		t.Fatalf("reload: %v", got)
	}
	if got.Prompt != "db def round-trip" || got.Recurrence != "0 8 * * *" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	// Importing the SAME export again with conflict=error must 409-equivalent
	// (error) since the name now exists.
	if _, err := importTaskDefinitions(ctx, st, bytes.NewReader(buf.Bytes()), "", false, models.TaskImportConflictError); err == nil {
		t.Fatal("re-import with conflict=error should fail on collision")
	}
	// conflict=replace updates in place.
	resp, err = importTaskDefinitions(ctx, st, bytes.NewReader(buf.Bytes()), "", false, models.TaskImportConflictReplace)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if resp.Replaced != 1 {
		t.Fatalf("replace: Replaced = %d, want 1", resp.Replaced)
	}
	_ = strings.TrimSpace
}
