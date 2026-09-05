package admincli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
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
	// existing are the rows this box already holds, keyed by id — the import
	// collision policy's target lookup (#1267).
	existing map[uuid.UUID]*models.Task
}

func (f *fakeTaskStore) ListScheduledTasks(context.Context) ([]*models.Task, error) {
	return f.scheduled, nil
}
func (f *fakeTaskStore) AddTaskWithContext(_ context.Context, t *models.Task) (*models.Task, error) {
	f.added = append(f.added, t)
	return t, nil
}
func (f *fakeTaskStore) GetTaskWithContext(_ context.Context, id uuid.UUID) (*models.Task, error) {
	return f.existing[id], nil
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
	good := &models.Task{Prompt: "do it", Status: models.TaskStatusScheduled, Recurrence: "0 9 * * *", MCPSelection: models.MCPSelection{{Server: "acme"}}}
	if err := validateImportedTask(good); err != nil {
		t.Fatalf("valid task rejected: %v", err)
	}
	cases := map[string]*models.Task{
		"empty prompt":       {Prompt: "  ", Status: models.TaskStatusScheduled},
		"bad cron":           {Prompt: "x", Status: models.TaskStatusScheduled, Recurrence: "not a cron"},
		"mcp without server": {Prompt: "x", Status: models.TaskStatusScheduled, MCPSelection: models.MCPSelection{{Account: "a"}}},
		// Transient statuses name lease/pause state no import can honor (#1267).
		"no status":      {Prompt: "x"},
		"leased":         {Prompt: "x", Status: models.TaskStatusLeased},
		"running":        {Prompt: "x", Status: models.TaskStatusRunning},
		"awaiting input": {Prompt: "x", Status: models.TaskStatusPausedAwaitingInput},
		"awaiting wake":  {Prompt: "x", Status: models.TaskStatusPausedAwaitingWake},
		"unknown status": {Prompt: "x", Status: models.TaskStatus("bogus")},
		"retired status": {Prompt: "x", Status: models.TaskStatus("analyzing")},
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
	if _, err := importTasks(context.Background(), f, bytes.NewReader(body), taskImportOpts{}); err == nil {
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
	if _, err := importTasks(context.Background(), f, &buf, taskImportOpts{}); err == nil {
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
	if _, err := importTasks(context.Background(), f, &buf, taskImportOpts{}); err != nil {
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
	if n, err := importTasks(context.Background(), dst, &buf, taskImportOpts{}); err != nil || n != 1 {
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
	if n, err := importTasks(ctx, st, &buf, taskImportOpts{}); err != nil || n != 1 {
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

// fakeTaskListStore is an in-memory taskListStore for DB-free tests of the
// `sched task list` output (#722).
type fakeTaskListStore struct {
	tasks     []*models.Task
	total     int
	gotFilter db.TaskFilter
	gotLimit  int
	gotOffset int
}

func (f *fakeTaskListStore) GetTasksFiltered(filter db.TaskFilter, limit, offset int) ([]*models.Task, int, error) {
	f.gotFilter, f.gotLimit, f.gotOffset = filter, limit, offset
	return f.tasks, f.total, nil
}

func TestListTasks_TableOutput(t *testing.T) {
	when := time.Date(2026, 7, 11, 9, 30, 0, 0, time.UTC)
	id1 := uuid.MustParse("aaaaaaaa-1111-2222-3333-444444444444")
	id2 := uuid.MustParse("bbbbbbbb-1111-2222-3333-444444444444")
	st := &fakeTaskListStore{
		tasks: []*models.Task{
			{ID: id1, Name: "nightly-report", Prompt: "long prompt", Status: models.TaskStatusScheduled,
				Priority: 50, Recurrence: "0 9 * * *", Model: ptr("z-ai/glm-5.2")},
			{ID: id2, Prompt: "summarize the\nweekly issues   backlog for the whole team please and thanks",
				Status: models.TaskStatusPending, Priority: 30, ScheduledFor: &when},
		},
		total: 2,
	}
	var buf, notes bytes.Buffer
	if err := listTasks(st, &buf, &notes, "", 50, false); err != nil {
		t.Fatalf("listTasks: %v", err)
	}
	out := buf.String()
	// Both rows fit: no "showing N of M" note anywhere, and nothing but the
	// table on stdout.
	if notes.Len() != 0 {
		t.Errorf("unexpected stderr note: %q", notes.String())
	}
	for _, want := range []string{
		"ID", "NAME/PROMPT", "STATUS", "PRI", "SCHEDULE", "MODEL",
		"aaaaaaaa", "nightly-report", "scheduled", "0 9 * * *", "z-ai/glm-5.2",
		"bbbbbbbb", "summarize the weekly issues backlog", "pending", "2026-07-11 09:30Z",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output should contain %q; got:\n%s", want, out)
		}
	}
	// The prompt excerpt must be collapsed to one line and truncated.
	if strings.Contains(out, "\nweekly") || strings.Contains(out, "please and thanks") {
		t.Errorf("prompt should be collapsed + truncated; got:\n%s", out)
	}
	if st.gotLimit != 50 || st.gotOffset != 0 || st.gotFilter.Status != nil {
		t.Errorf("unexpected query: filter=%+v limit=%d offset=%d", st.gotFilter, st.gotLimit, st.gotOffset)
	}
}

func TestListTasks_StatusFilterAndJSON(t *testing.T) {
	id := uuid.New()
	st := &fakeTaskListStore{
		tasks: []*models.Task{{ID: id, Prompt: "p", Status: models.TaskStatusRunning, Priority: 10}},
		total: 1,
	}
	var buf bytes.Buffer
	if err := listTasks(st, &buf, io.Discard, "running", 5, true); err != nil {
		t.Fatalf("listTasks: %v", err)
	}
	if st.gotFilter.Status == nil || *st.gotFilter.Status != "running" || st.gotLimit != 5 {
		t.Errorf("filter not threaded: %+v limit=%d", st.gotFilter, st.gotLimit)
	}
	var decoded []*models.Task
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("json output should decode as a task array: %v\n%s", err, buf.String())
	}
	if len(decoded) != 1 || decoded[0].ID != id {
		t.Errorf("json output mismatch: %+v", decoded)
	}
}

// TestListTasks_NotesGoToErrW — the "no tasks match" / "showing N of M" notes
// go to the injected errW (stderr in production), never into the table stream,
// so `fleet sched task list | wc -l` counts rows and a redirected listing has
// no stray prose. They used to be written straight to os.Stderr, which this
// test could not observe.
func TestListTasks_NotesGoToErrW(t *testing.T) {
	var out, errW bytes.Buffer
	if err := listTasks(&fakeTaskListStore{}, &out, &errW, "", 50, false); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 || !strings.Contains(errW.String(), "no tasks match") {
		t.Errorf("empty list: stdout=%q stderr=%q", out.String(), errW.String())
	}

	out.Reset()
	errW.Reset()
	st := &fakeTaskListStore{
		tasks: []*models.Task{{ID: uuid.New(), Prompt: "p", Status: models.TaskStatusRunning}},
		total: 7,
	}
	if err := listTasks(st, &out, &errW, "", 1, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errW.String(), "showing 1 of 7 task(s)") {
		t.Errorf("truncation note missing from errW: %q", errW.String())
	}
	if strings.Contains(out.String(), "showing") {
		t.Errorf("truncation note leaked into stdout: %q", out.String())
	}
}

// TestRenderTable pins the shared list renderer: a header row first, one line
// per row, cells aligned by tabwriter (so a column never runs into the next).
func TestRenderTable(t *testing.T) {
	var buf bytes.Buffer
	err := renderTable(&buf, []string{"A", "BB"}, [][]string{{"x", "1"}, {"longer", "22"}})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want header + 2 rows, got %d lines:\n%s", len(lines), buf.String())
	}
	if !strings.HasPrefix(lines[0], "A") || !strings.Contains(lines[0], "BB") {
		t.Errorf("header = %q", lines[0])
	}
	// Aligned: the second column starts at the same offset on every line.
	col := strings.Index(lines[0], "BB")
	for _, l := range lines[1:] {
		if len(l) <= col || strings.Contains(l[:col], "\t") {
			t.Errorf("row not aligned/tab-free: %q", l)
		}
	}
	if strings.Index(lines[1], "1") != col || strings.Index(lines[2], "22") != col {
		t.Errorf("second column misaligned:\n%s", buf.String())
	}
}

func TestValidTaskStatusFilter(t *testing.T) {
	for _, ok := range []string{"scheduled", "pending", "running", "success", "error", "cancelled", "dead_lettered", "paused_awaiting_input"} {
		if !validTaskStatusFilter(ok) {
			t.Errorf("%q should be a valid status filter", ok)
		}
	}
	for _, bad := range []string{"complete", "SCHEDULED", "queued", "deadletter"} {
		if validTaskStatusFilter(bad) {
			t.Errorf("%q should be rejected", bad)
		}
	}
}
