package admincli

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// fakeTaskStore is an in-memory taskStore for deterministic, DB-free tests of the
// export/import seam.
type fakeTaskStore struct {
	scheduled []*models.Task
	present   map[uuid.UUID]string // users "present on the target box"
	added     []*models.Task
}

func (f *fakeTaskStore) ListScheduledTasks(context.Context) ([]*models.Task, error) {
	return f.scheduled, nil
}
func (f *fakeTaskStore) AddTaskWithContext(_ context.Context, t *models.Task) (*models.Task, error) {
	f.added = append(f.added, t)
	return t, nil
}
func (f *fakeTaskStore) GetUsersByIDsWithContext(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	out := map[uuid.UUID]string{}
	for _, id := range ids {
		if name, ok := f.present[id]; ok {
			out[id] = name
		}
	}
	return out, nil
}

func ptr[T any](v T) *T { return &v }

func TestValidateImportedTask(t *testing.T) {
	good := &models.Task{Prompt: "do it", Recurrence: "0 9 * * *", MCPSelection: models.MCPSelection{{Server: "acme"}}}
	if err := validateImportedTask(good); err != nil {
		t.Fatalf("valid task rejected: %v", err)
	}
	cases := map[string]*models.Task{
		"empty prompt":       {Prompt: "  "},
		"bad cron":           {Prompt: "x", Recurrence: "not a cron"},
		"mcp without server": {Prompt: "x", MCPSelection: models.MCPSelection{{Account: "a"}}},
	}
	for name, tk := range cases {
		if err := validateImportedTask(tk); err == nil {
			t.Errorf("%s: expected validation error, got nil", name)
		}
	}
}

func TestImportTasks_RejectsUnknownVersion(t *testing.T) {
	f := &fakeTaskStore{}
	body := []byte(`{"version":99,"tasks":[]}`)
	if _, err := importTasks(context.Background(), f, bytes.NewReader(body)); err == nil {
		t.Fatal("expected an unsupported-version error")
	}
	if len(f.added) != 0 {
		t.Fatal("nothing should be inserted for an unknown version")
	}
}

// A bad payload must insert NOTHING even if a valid task precedes the invalid one
// (validate-all-before-insert).
func TestImportTasks_BadPayloadInsertsNothing(t *testing.T) {
	f := &fakeTaskStore{}
	env := taskExportEnvelope{Version: taskExportVersion, Tasks: []*models.Task{
		{ID: uuid.New(), Prompt: "ok", Status: models.TaskStatusScheduled},
		{ID: uuid.New(), Prompt: "bad", Recurrence: "nope", Status: models.TaskStatusScheduled},
	}}
	var buf bytes.Buffer
	if _, err := exportTasksEnvelope(&buf, env); err != nil {
		t.Fatal(err)
	}
	if _, err := importTasks(context.Background(), f, &buf); err == nil {
		t.Fatal("expected import to fail on the bad recurrence")
	}
	if len(f.added) != 0 {
		t.Fatalf("a bad payload must insert nothing; got %d adds", len(f.added))
	}
}

func TestImportTasks_NullsMissingCreatedBy(t *testing.T) {
	absent := uuid.New()
	present := uuid.New()
	f := &fakeTaskStore{present: map[uuid.UUID]string{present: "alice"}}
	env := taskExportEnvelope{Version: taskExportVersion, Tasks: []*models.Task{
		{ID: uuid.New(), Prompt: "a", CreatedBy: &absent, Status: models.TaskStatusScheduled},
		{ID: uuid.New(), Prompt: "b", CreatedBy: &present, Status: models.TaskStatusScheduled},
	}}
	var buf bytes.Buffer
	if _, err := exportTasksEnvelope(&buf, env); err != nil {
		t.Fatal(err)
	}
	if _, err := importTasks(context.Background(), f, &buf); err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(f.added) != 2 {
		t.Fatalf("added %d, want 2", len(f.added))
	}
	if f.added[0].CreatedBy != nil {
		t.Error("created_by referencing an absent user must be nulled (FK safety)")
	}
	if f.added[1].CreatedBy == nil || *f.added[1].CreatedBy != present {
		t.Error("created_by referencing a present user must be preserved")
	}
}

// TestExportImportRoundTrip_Fake proves the full export→import seam preserves
// every Task field through JSON (the core acceptance), without a DB.
func TestExportImportRoundTrip_Fake(t *testing.T) {
	orig := &models.Task{
		ID:                     uuid.New(),
		Prompt:                 "generate the weekly report",
		Model:                  ptr("anthropic/claude-opus-4.8"),
		FallbackModel:          ptr("anthropic/claude-sonnet-4.6"),
		MaxIterations:          ptr(42),
		MCPSelection:           models.MCPSelection{{Server: "sendgrid", Account: "client_a"}},
		Priority:               7,
		InstructionSelfImprove: true,
		Status:                 models.TaskStatusScheduled,
		CreatedAt:              time.Now().UTC().Truncate(time.Microsecond),
		ScheduledFor:           ptr(time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)),
		Recurrence:             "0 9 * * 1",
		Files:                  []string{"a.csv", "b.csv"},
	}
	src := &fakeTaskStore{scheduled: []*models.Task{orig}}

	var buf bytes.Buffer
	if n, err := exportTasks(context.Background(), src, &buf); err != nil || n != 1 {
		t.Fatalf("export n=%d err=%v", n, err)
	}

	dst := &fakeTaskStore{}
	if n, err := importTasks(context.Background(), dst, &buf); err != nil || n != 1 {
		t.Fatalf("import n=%d err=%v", n, err)
	}
	got := dst.added[0]
	if got.ID != orig.ID || got.Prompt != orig.Prompt || got.Priority != orig.Priority ||
		got.InstructionSelfImprove != orig.InstructionSelfImprove || got.Status != orig.Status ||
		got.Recurrence != orig.Recurrence {
		t.Fatalf("scalar fields not preserved: got %+v", got)
	}
	if *got.Model != *orig.Model || *got.FallbackModel != *orig.FallbackModel || *got.MaxIterations != *orig.MaxIterations {
		t.Fatal("pointer fields not preserved")
	}
	if len(got.MCPSelection) != 1 || got.MCPSelection[0].Server != "sendgrid" || got.MCPSelection[0].Account != "client_a" {
		t.Fatalf("mcp_selection not preserved: %+v", got.MCPSelection)
	}
	if !got.CreatedAt.Equal(orig.CreatedAt) {
		t.Errorf("created_at not preserved: got %v want %v", got.CreatedAt, orig.CreatedAt)
	}
	if len(got.Files) != 2 || got.Files[0] != "a.csv" {
		t.Errorf("files not preserved: %v", got.Files)
	}
}

// exportTasksEnvelope encodes a pre-built envelope (test helper so the bad/version
// cases can craft payloads the production exporter wouldn't emit).
func exportTasksEnvelope(buf *bytes.Buffer, env taskExportEnvelope) (int, error) {
	src := &fakeTaskStore{scheduled: env.Tasks}
	return exportTasks(context.Background(), src, buf)
}

// TestExportImportRoundTrip_DB is the end-to-end proof against the real sched DB
// (gated on DATABASE_URL — the sched-suite convention; skips when absent).
func TestExportImportRoundTrip_DB(t *testing.T) {
	database := db.New()
	if err := database.Init("", db.DefaultPoolConfig()); err != nil {
		t.Skipf("sched DB unavailable: %v", err)
	}
	ctx := context.Background()
	conn, err := database.Conn().Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock(1)"); err != nil {
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
		conn.ExecContext(ctx, "SELECT pg_advisory_unlock(1)")
		conn.Close()
		database.Close()
	})

	st := storage.New()
	st.SetDatabase(database)

	seed := &models.Task{
		ID:            uuid.New(),
		Prompt:        "db round-trip",
		Model:         ptr("anthropic/claude-opus-4.8"),
		MaxIterations: ptr(5),
		MCPSelection:  models.MCPSelection{{Server: "sendgrid"}},
		Priority:      3,
		Status:        models.TaskStatusScheduled,
		CreatedAt:     time.Now().UTC().Truncate(time.Microsecond),
		Recurrence:    "0 8 * * *",
	}
	if _, err := st.AddTaskWithContext(ctx, seed); err != nil {
		t.Fatalf("seed AddTask: %v", err)
	}

	var buf bytes.Buffer
	if _, err := exportTasks(ctx, st, &buf); err != nil {
		t.Fatalf("export: %v", err)
	}
	clean() // wipe, then import the export back
	if n, err := importTasks(ctx, st, &buf); err != nil || n != 1 {
		t.Fatalf("import n=%d err=%v", n, err)
	}

	got, err := st.ListScheduledTasks(ctx)
	if err != nil || len(got) != 1 {
		t.Fatalf("reload got %d tasks, err=%v", len(got), err)
	}
	if got[0].ID != seed.ID || got[0].Prompt != seed.Prompt || got[0].Recurrence != seed.Recurrence ||
		got[0].Priority != seed.Priority || *got[0].Model != *seed.Model {
		t.Fatalf("DB round-trip did not preserve fields: %+v", got[0])
	}
}
