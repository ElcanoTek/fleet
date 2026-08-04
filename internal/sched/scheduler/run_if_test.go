package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// newTestScheduler mirrors storage.newTestStore but for the scheduler package:
// it skips when no DB is available (the sched integration suite uses a real
// PostgreSQL instance via DATABASE_URL), shares the advisory lock so it does
// not race the storage suite, and returns a ready scheduler + the underlying
// storage so the test can drive ProcessScheduledTasks and inspect the rows.
func newTestScheduler(t *testing.T) (*Scheduler, *storage.Storage) {
	t.Helper()
	database := db.New()
	if err := database.Init("", db.DefaultPoolConfig()); err != nil {
		t.Skipf("Skipping scheduler test because DB init failed: %v", err)
	}

	ctx := context.Background()
	conn, err := database.Conn().Conn(ctx)
	if err != nil {
		t.Fatalf("Failed to get DB connection for lock: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock(1)"); err != nil {
		conn.Close()
		t.Fatalf("Failed to acquire test lock: %v", err)
	}
	t.Cleanup(func() {
		if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_unlock(1)"); err != nil {
			t.Logf("Failed to release test lock: %v", err)
		}
		conn.Close()
	})

	cleanup := func() {
		for _, q := range []string{"DELETE FROM logs", "DELETE FROM tasks", "DELETE FROM users"} {
			database.Conn().ExecContext(ctx, q)
		}
	}
	cleanup()
	t.Cleanup(cleanup)

	store := storage.New()
	store.SetDatabase(database)
	store.SetTimezone("UTC")
	t.Cleanup(func() { database.Close() })

	return New(store, "UTC"), store
}

// TestEvalRunIfExitCodeIs pins the host-side gate evaluation (#269): the task
// runs iff the command exits with ExitCodeIs, and a failing check returns a
// non-empty reason with the captured stderr. The check runs with the restricted
// PATH so a bare `false` / `true` is reachable via /usr/bin or /bin.
func TestEvalRunIfExitCodeIs(t *testing.T) {
	s, _ := newTestScheduler(t)

	// `true` exits 0; with ExitCodeIs=0 the task should run.
	task := &models.Task{RunIf: &models.RunIf{Command: "true", ExitCodeIs: 0, TimeoutSeconds: 5}}
	ok, reason, err := s.evalRunIf(task)
	if err != nil {
		t.Fatalf("evalRunIf(true): unexpected err %v", err)
	}
	if !ok {
		t.Errorf("evalRunIf(true): expected ok, got reason %q", reason)
	}

	// `false` exits 1; with ExitCodeIs=0 the task should NOT run.
	task = &models.Task{RunIf: &models.RunIf{Command: "false", ExitCodeIs: 0, TimeoutSeconds: 5}}
	ok, reason, err = s.evalRunIf(task)
	if err != nil {
		t.Fatalf("evalRunIf(false): unexpected err %v", err)
	}
	if ok {
		t.Error("evalRunIf(false): expected skip, got ok")
	}
	if reason == "" {
		t.Error("evalRunIf(false): expected non-empty reason")
	}

	// An inverted gate: ExitCodeIs=1 means "run only when the command fails".
	task = &models.Task{RunIf: &models.RunIf{Command: "false", ExitCodeIs: 1, TimeoutSeconds: 5}}
	ok, _, err = s.evalRunIf(task)
	if err != nil {
		t.Fatalf("evalRunIf(false, want 1): unexpected err %v", err)
	}
	if !ok {
		t.Error("evalRunIf(false, want 1): expected ok")
	}

	// A stderr-bearing failure surfaces the captured stderr in the reason.
	task = &models.Task{RunIf: &models.RunIf{Command: "echo oops >&2; exit 3", ExitCodeIs: 0, TimeoutSeconds: 5}}
	ok, reason, err = s.evalRunIf(task)
	if err != nil {
		t.Fatalf("evalRunIf(echo+exit3): unexpected err %v", err)
	}
	if ok {
		t.Error("evalRunIf(echo+exit3): expected skip")
	}
	if !strings.Contains(reason, "oops") {
		t.Errorf("evalRunIf(echo+exit3): reason %q must contain captured stderr", reason)
	}

	// A noisy gate must not grow Fleet's heap without bound while it runs. The
	// writer keeps draining after the retained prefix fills, so the command can
	// still exit normally and the reason makes truncation explicit.
	task = &models.Task{RunIf: &models.RunIf{Command: "yes x | head -c 20000 >&2; exit 3", ExitCodeIs: 0, TimeoutSeconds: 5}}
	ok, reason, err = s.evalRunIf(task)
	if err != nil {
		t.Fatalf("evalRunIf(noisy stderr): unexpected err %v", err)
	}
	if ok {
		t.Error("evalRunIf(noisy stderr): expected skip")
	}
	if !strings.Contains(reason, runIfStderrTruncated) {
		t.Errorf("evalRunIf(noisy stderr): reason must mark truncation, length=%d", len(reason))
	}
	if len(reason) > maxRunIfStderrBytes+128 {
		t.Errorf("evalRunIf(noisy stderr): reason length=%d, want bounded near %d", len(reason), maxRunIfStderrBytes)
	}
}

func TestCappedRunIfStderrConsumesAfterCap(t *testing.T) {
	var got cappedRunIfStderr
	input := strings.Repeat("x", maxRunIfStderrBytes+100)
	n, err := got.Write([]byte(input))
	if err != nil || n != len(input) {
		t.Fatalf("Write = %d, %v; want %d, nil", n, err, len(input))
	}
	if got.buf.Len() != maxRunIfStderrBytes {
		t.Fatalf("retained bytes = %d, want %d", got.buf.Len(), maxRunIfStderrBytes)
	}
	if !strings.HasSuffix(got.String(), runIfStderrTruncated) {
		t.Fatalf("String() must carry truncation marker")
	}
}

// TestEvalRunIfTimeout pins the hard wall-clock timeout (#269): a `sleep` that
// outlasts the gate's timeout returns a DeadlineExceeded error, which the
// scheduler's ProcessScheduledTasks routes via the on_error policy.
func TestEvalRunIfTimeout(t *testing.T) {
	s, _ := newTestScheduler(t)
	task := &models.Task{RunIf: &models.RunIf{Command: "sleep 10", ExitCodeIs: 0, TimeoutSeconds: 1}}
	ok, _, err := s.evalRunIf(task)
	if err == nil {
		t.Fatal("evalRunIf(sleep 10, timeout 1s): expected timeout error, got nil")
	}
	if ok {
		t.Error("evalRunIf(sleep 10, timeout 1s): must not return ok on timeout")
	}
}

// TestEvalRunIfBackgroundChildDoesNotHang pins the group-kill + WaitDelay
// hardening: a gate that exits immediately but leaves a background child
// holding the stderr pipe must return promptly with the gate's own exit
// status, instead of blocking cmd.Run (and the sequential scheduler tick)
// until the child exits. Mirrors the host sandbox's bash regression test.
func TestEvalRunIfBackgroundChildDoesNotHang(t *testing.T) {
	s, _ := newTestScheduler(t)

	// Child outlives the gate but not its timeout: WaitDelay must force the
	// pipe closed and the gate's clean exit 0 must still count as "run". The
	// backgrounded sleep inherits the gate's stderr pipe and holds it open.
	start := time.Now()
	task := &models.Task{RunIf: &models.RunIf{Command: "sleep 45 & exit 0", ExitCodeIs: 0, TimeoutSeconds: 60}}
	ok, reason, err := s.evalRunIf(task)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("evalRunIf(background child): unexpected err %v", err)
	}
	if !ok {
		t.Errorf("evalRunIf(background child): expected ok (gate exited 0), got reason %q", reason)
	}
	if elapsed > 25*time.Second {
		t.Fatalf("evalRunIf blocked %v on a background child's pipe; WaitDelay regression", elapsed)
	}

	// Child outlives the TIMEOUT: the process-group SIGKILL must reap it at
	// the deadline so the check errors at ~timeout, not at the child's exit.
	start = time.Now()
	task = &models.Task{RunIf: &models.RunIf{Command: "sleep 60 & exit 0", ExitCodeIs: 0, TimeoutSeconds: 2}}
	_, _, err = s.evalRunIf(task)
	elapsed = time.Since(start)
	if err == nil {
		t.Fatal("evalRunIf(child past timeout): expected timeout error")
	}
	if elapsed > 20*time.Second {
		t.Fatalf("evalRunIf blocked %v past its 2s timeout; group-kill regression", elapsed)
	}
}

// TestHandleSkipLogLineIsJSON pins the task_skipped log record against
// forgery: the reason carries raw gate stderr, so an embedded newline or
// quote must not split the record or inject fields. The logged reason is
// clamped; the full text still persists to last_skip_reason.
func TestHandleSkipLogLineIsJSON(t *testing.T) {
	s, store := newTestScheduler(t)
	ctx := context.Background()

	past := time.Now().UTC().Add(-1 * time.Hour)
	task := &models.Task{
		ID: uuid.New(), Prompt: "gated", Status: models.TaskStatusScheduled, CreatedAt: time.Now().UTC(),
		ScheduledFor: &past,
		RunIf:        &models.RunIf{Command: "false", ExitCodeIs: 0, TimeoutSeconds: 5},
	}
	if _, err := store.AddTaskWithContext(ctx, task); err != nil {
		t.Fatalf("add task: %v", err)
	}

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	reason := "exit 3 (want 0): line one\nline two \"quoted\", {\"event\":\"forged\",\"task_id\":\"x\"}"
	s.handleSkip(task, reason)

	var logged struct {
		Event     string `json:"event"`
		TaskID    string `json:"task_id"`
		Reason    string `json:"reason"`
		NextRunAt string `json:"next_run_at"`
	}
	var found bool
	for _, line := range strings.Split(buf.String(), "\n") {
		idx := strings.Index(line, "{")
		if idx < 0 || !strings.Contains(line, "task_skipped") {
			continue
		}
		found = true
		if err := json.Unmarshal([]byte(line[idx:]), &logged); err != nil {
			t.Fatalf("task_skipped log line is not valid JSON: %v\nline: %q", err, line)
		}
	}
	if !found {
		t.Fatalf("no task_skipped log line emitted; log: %q", buf.String())
	}
	if logged.Event != "task_skipped" || logged.TaskID != task.ID.String() {
		t.Errorf("logged event = %+v, want task_skipped for %s", logged, task.ID)
	}
	if logged.Reason != reason {
		t.Errorf("logged reason = %q, want the raw reason escaped intact", logged.Reason)
	}

	// A reason larger than the log clamp is truncated in the LOG line only;
	// last_skip_reason keeps the full text for diagnosis.
	buf.Reset()
	longReason := "exit 3 (want 0): " + strings.Repeat("x", 4096)
	s.handleSkip(task, longReason)
	for _, line := range strings.Split(buf.String(), "\n") {
		idx := strings.Index(line, "{")
		if idx < 0 || !strings.Contains(line, "task_skipped") {
			continue
		}
		if err := json.Unmarshal([]byte(line[idx:]), &logged); err != nil {
			t.Fatalf("clamped task_skipped line is not valid JSON: %v", err)
		}
	}
	if len(logged.Reason) > maxSkipReasonLogBytes+64 {
		t.Errorf("logged reason length = %d, want clamped near %d", len(logged.Reason), maxSkipReasonLogBytes)
	}
	if !strings.HasSuffix(logged.Reason, "…[truncated]") {
		t.Errorf("clamped reason must carry the truncation marker, got %q…", logged.Reason[:64])
	}
	got, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.LastSkipReason == nil || *got.LastSkipReason != longReason {
		t.Error("last_skip_reason must keep the full (uncapped-by-the-log-clamp) reason")
	}
}

// TestProcessScheduledTasksSkipsFailingGate pins the end-to-end skip path
// (#269): a recurring task whose gate always fails (`false`) is NEVER promoted
// to pending across multiple ticks, and skip_count increments. An acceptance
// criterion from the issue.
func TestProcessScheduledTasksSkipsFailingGate(t *testing.T) {
	s, store := newTestScheduler(t)
	ctx := context.Background()

	past := time.Now().UTC().Add(-1 * time.Hour)
	gated := &models.Task{
		ID: uuid.New(), Prompt: "gated", Status: models.TaskStatusScheduled, CreatedAt: time.Now().UTC(),
		Recurrence: "@hourly", Timezone: "UTC", ScheduledFor: &past,
		RunIf: &models.RunIf{Command: "false", ExitCodeIs: 0, TimeoutSeconds: 5, OnError: models.RunIfOnErrorRun},
	}
	if _, err := store.AddTaskWithContext(ctx, gated); err != nil {
		t.Fatalf("add gated: %v", err)
	}
	// A plain scheduled task (no gate) alongside it must still be promoted.
	plain := &models.Task{
		ID: uuid.New(), Prompt: "plain", Status: models.TaskStatusScheduled, CreatedAt: time.Now().UTC(),
		ScheduledFor: &past,
	}
	if _, err := store.AddTaskWithContext(ctx, plain); err != nil {
		t.Fatalf("add plain: %v", err)
	}

	s.ProcessScheduledTasks()

	// The plain task was promoted to pending; the gated task was skipped.
	plainGot, err := store.GetTask(plain.ID)
	if err != nil {
		t.Fatalf("get plain: %v", err)
	}
	if plainGot.Status != models.TaskStatusPending {
		t.Errorf("plain task status = %s, want pending (gate must not affect ungated tasks)", plainGot.Status)
	}
	gatedGot, err := store.GetTask(gated.ID)
	if err != nil {
		t.Fatalf("get gated: %v", err)
	}
	if gatedGot.Status != models.TaskStatusScheduled {
		t.Errorf("gated task status = %s, want scheduled (failing gate must not promote)", gatedGot.Status)
	}
	if gatedGot.SkipCount != 1 {
		t.Errorf("gated task skip_count = %d, want 1", gatedGot.SkipCount)
	}
	if gatedGot.LastSkipAt == nil || gatedGot.LastSkipReason == nil {
		t.Error("gated task must stamp last_skip_at + last_skip_reason")
	}
	// scheduled_for must advance to the next cron tick (in the future).
	if gatedGot.ScheduledFor == nil || !gatedGot.ScheduledFor.After(time.Now().UTC()) {
		t.Errorf("gated task scheduled_for = %v, must advance to a future cron tick", gatedGot.ScheduledFor)
	}

	// A second tick must NOT promote the gated task (its scheduled_for is now
	// in the future) and must NOT double-count the skip. Re-due it first to
	// simulate the cron catching up, then run again.
	future := gatedGot.ScheduledFor.Add(-2 * time.Hour)
	_ = store.DB().Conn().QueryRowContext(ctx, "UPDATE tasks SET scheduled_for = $1 WHERE id = $2", future, gated.ID).Err()
	s.ProcessScheduledTasks()
	gatedGot, _ = store.GetTask(gated.ID)
	if gatedGot.Status != models.TaskStatusScheduled {
		t.Errorf("gated task status after 2nd tick = %s, want scheduled", gatedGot.Status)
	}
	if gatedGot.SkipCount != 2 {
		t.Errorf("gated task skip_count after 2nd tick = %d, want 2", gatedGot.SkipCount)
	}
}

// TestProcessScheduledTasksOnErrorRun pins the on_error:"run" policy (#269):
// a gate that times out (a check error) with on_error:"run" must promote the
// task anyway, NOT skip it. An acceptance criterion from the issue.
func TestProcessScheduledTasksOnErrorRun(t *testing.T) {
	s, store := newTestScheduler(t)
	ctx := context.Background()

	past := time.Now().UTC().Add(-1 * time.Hour)
	gated := &models.Task{
		ID: uuid.New(), Prompt: "gated", Status: models.TaskStatusScheduled, CreatedAt: time.Now().UTC(),
		Recurrence: "@hourly", Timezone: "UTC", ScheduledFor: &past,
		// A sleep that outlasts the timeout -> check error -> on_error:"run" -> promote.
		RunIf: &models.RunIf{Command: "sleep 10", ExitCodeIs: 0, TimeoutSeconds: 1, OnError: models.RunIfOnErrorRun},
	}
	if _, err := store.AddTaskWithContext(ctx, gated); err != nil {
		t.Fatalf("add gated: %v", err)
	}

	s.ProcessScheduledTasks()

	got, err := store.GetTask(gated.ID)
	if err != nil {
		t.Fatalf("get gated: %v", err)
	}
	if got.Status != models.TaskStatusPending {
		t.Errorf("on_error=run task status = %s, want pending (check error must promote)", got.Status)
	}
	if got.SkipCount != 0 {
		t.Errorf("on_error=run task skip_count = %d, want 0 (must not skip)", got.SkipCount)
	}
}

// TestProcessScheduledTasksOnErrorSkip pins the on_error:"skip" policy (#269):
// a gate that times out with on_error:"skip" must skip the task (not promote).
func TestProcessScheduledTasksOnErrorSkip(t *testing.T) {
	s, store := newTestScheduler(t)
	ctx := context.Background()

	past := time.Now().UTC().Add(-1 * time.Hour)
	gated := &models.Task{
		ID: uuid.New(), Prompt: "gated", Status: models.TaskStatusScheduled, CreatedAt: time.Now().UTC(),
		Recurrence: "@hourly", Timezone: "UTC", ScheduledFor: &past,
		RunIf: &models.RunIf{Command: "sleep 10", ExitCodeIs: 0, TimeoutSeconds: 1, OnError: models.RunIfOnErrorSkip},
	}
	if _, err := store.AddTaskWithContext(ctx, gated); err != nil {
		t.Fatalf("add gated: %v", err)
	}

	s.ProcessScheduledTasks()

	got, err := store.GetTask(gated.ID)
	if err != nil {
		t.Fatalf("get gated: %v", err)
	}
	if got.Status != models.TaskStatusScheduled {
		t.Errorf("on_error=skip task status = %s, want scheduled (check error must skip)", got.Status)
	}
	if got.SkipCount != 1 {
		t.Errorf("on_error=skip task skip_count = %d, want 1", got.SkipCount)
	}
}
