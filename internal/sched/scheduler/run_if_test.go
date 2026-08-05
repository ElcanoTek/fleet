package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
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
	s.gateWG.Wait() // gates settle asynchronously

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
	s.gateWG.Wait()
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
	s.gateWG.Wait() // gates settle asynchronously

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
	s.gateWG.Wait() // gates settle asynchronously

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

// TestGateSkipBackoff pins the declined-gate retry schedule: 30s doubling per
// recorded skip, capped at 30m — so a permanently-declining one-shot gate
// settles at ~2 host commands per hour instead of one per 30s tick forever.
func TestGateSkipBackoff(t *testing.T) {
	cases := []struct {
		skips int
		want  time.Duration
	}{
		{0, 30 * time.Second},
		{1, time.Minute},
		{2, 2 * time.Minute},
		{5, 16 * time.Minute},
		{6, 30 * time.Minute},  // 32m capped
		{50, 30 * time.Minute}, // stays at the cap, no overflow
	}
	for _, c := range cases {
		if got := gateSkipBackoff(c.skips); got != c.want {
			t.Errorf("gateSkipBackoff(%d) = %v, want %v", c.skips, got, c.want)
		}
	}
}

// TestProcessScheduledTasksNotBlockedBySlowGate pins the async-evaluation
// hardening: a slow gate must not stall the tick. Before the fix the tick ran
// every gate inline, so one admin-authored gate pointing at a hung dependency
// (timeout up to 300s) delayed ALL scheduling and lease recovery by its full
// runtime. The tick must return promptly and promote ungated tasks while the
// gate is still running; the gated task settles asynchronously.
func TestProcessScheduledTasksNotBlockedBySlowGate(t *testing.T) {
	s, store := newTestScheduler(t)
	ctx := context.Background()

	past := time.Now().UTC().Add(-time.Minute)
	slow := &models.Task{
		ID: uuid.New(), Prompt: "slow gate", Status: models.TaskStatusScheduled, CreatedAt: time.Now().UTC(),
		ScheduledFor: &past,
		RunIf:        &models.RunIf{Command: "sleep 3", ExitCodeIs: 0, TimeoutSeconds: 30},
	}
	plain := &models.Task{
		ID: uuid.New(), Prompt: "plain", Status: models.TaskStatusScheduled, CreatedAt: time.Now().UTC(),
		ScheduledFor: &past,
	}
	for _, task := range []*models.Task{slow, plain} {
		if _, err := store.AddTaskWithContext(ctx, task); err != nil {
			t.Fatalf("add task: %v", err)
		}
	}

	start := time.Now()
	s.ProcessScheduledTasks()
	elapsed := time.Since(start)
	if elapsed > 2500*time.Millisecond {
		t.Fatalf("ProcessScheduledTasks blocked %v on a 3s gate; the tick must not wait on gate evaluation", elapsed)
	}
	// The ungated task is already pending, before the gate settles.
	plainGot, err := store.GetTask(plain.ID)
	if err != nil {
		t.Fatalf("get plain: %v", err)
	}
	if plainGot.Status != models.TaskStatusPending {
		t.Errorf("plain task status = %s, want pending while the gate still runs", plainGot.Status)
	}

	s.gateWG.Wait()
	slowGot, err := store.GetTask(slow.ID)
	if err != nil {
		t.Fatalf("get slow: %v", err)
	}
	if slowGot.Status != models.TaskStatusPending {
		t.Errorf("slow-gated task status = %s, want pending after its passing gate settles", slowGot.Status)
	}
}

// TestGateEvalNotDoubleDispatchedWhileInFlight pins the in-flight dedupe: a
// task whose gate is still running when the next tick re-fetches it (it is
// still scheduled and due) must NOT have its gate started a second time.
func TestGateEvalNotDoubleDispatchedWhileInFlight(t *testing.T) {
	s, store := newTestScheduler(t)
	ctx := context.Background()

	marker := filepath.Join(t.TempDir(), "gate-runs")
	past := time.Now().UTC().Add(-time.Minute)
	gated := &models.Task{
		ID: uuid.New(), Prompt: "gated", Status: models.TaskStatusScheduled, CreatedAt: time.Now().UTC(),
		ScheduledFor: &past,
		RunIf:        &models.RunIf{Command: "echo run >> " + marker + "; sleep 2; exit 1", ExitCodeIs: 0, TimeoutSeconds: 30},
	}
	if _, err := store.AddTaskWithContext(ctx, gated); err != nil {
		t.Fatalf("add gated: %v", err)
	}

	s.ProcessScheduledTasks() // dispatches the gate
	s.ProcessScheduledTasks() // gate still in flight: must not dispatch again
	s.gateWG.Wait()

	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if runs := strings.Count(string(data), "run"); runs != 1 {
		t.Errorf("gate command ran %d times across two ticks, want exactly 1 (in-flight dedupe)", runs)
	}
	got, err := store.GetTask(gated.ID)
	if err != nil {
		t.Fatalf("get gated: %v", err)
	}
	if got.SkipCount != 1 {
		t.Errorf("skip_count = %d, want 1", got.SkipCount)
	}
}

// TestGatePoolFullDefersToNextTick pins the pool bound: with every evaluation
// slot busy, a due gated task is left scheduled (untouched) for a later tick
// rather than spawning an unbounded goroutine — and the next tick picks it up
// once a slot frees.
func TestGatePoolFullDefersToNextTick(t *testing.T) {
	s, store := newTestScheduler(t)
	s.gateSlots = make(chan struct{}, 1) // shrink the pool to one slot
	ctx := context.Background()

	past := time.Now().UTC().Add(-time.Minute)
	mk := func(prompt string) *models.Task {
		ts := past
		task := &models.Task{
			ID: uuid.New(), Prompt: prompt, Status: models.TaskStatusScheduled, CreatedAt: time.Now().UTC(),
			ScheduledFor: &ts,
			RunIf:        &models.RunIf{Command: "sleep 1; exit 1", ExitCodeIs: 0, TimeoutSeconds: 30},
		}
		if _, err := store.AddTaskWithContext(ctx, task); err != nil {
			t.Fatalf("add %s: %v", prompt, err)
		}
		return task
	}
	a, b := mk("gated a"), mk("gated b")

	s.ProcessScheduledTasks() // slot 1 taken; the other task is deferred
	s.gateWG.Wait()

	gotA, _ := store.GetTask(a.ID)
	gotB, _ := store.GetTask(b.ID)
	if gotA.SkipCount+gotB.SkipCount != 1 {
		t.Fatalf("one gate must settle this tick, got skips a=%d b=%d", gotA.SkipCount, gotB.SkipCount)
	}

	s.ProcessScheduledTasks() // the deferred task is still due and gets the slot
	s.gateWG.Wait()
	gotA, _ = store.GetTask(a.ID)
	gotB, _ = store.GetTask(b.ID)
	if gotA.SkipCount != 1 || gotB.SkipCount != 1 {
		t.Errorf("deferred gate must settle on the next tick, got skips a=%d b=%d", gotA.SkipCount, gotB.SkipCount)
	}
}

// TestDeclinedOneShotGateBacksOff pins the circuit breaker: a declining
// one-shot gate must advance scheduled_for by the skip-count backoff instead
// of staying due and re-running its host command every 30s tick forever.
func TestDeclinedOneShotGateBacksOff(t *testing.T) {
	s, store := newTestScheduler(t)
	ctx := context.Background()

	past := time.Now().UTC().Add(-time.Minute)
	gated := &models.Task{
		ID: uuid.New(), Prompt: "one-shot", Status: models.TaskStatusScheduled, CreatedAt: time.Now().UTC(),
		ScheduledFor: &past,
		RunIf:        &models.RunIf{Command: "false", ExitCodeIs: 0, TimeoutSeconds: 5},
	}
	if _, err := store.AddTaskWithContext(ctx, gated); err != nil {
		t.Fatalf("add gated: %v", err)
	}

	s.ProcessScheduledTasks()
	s.gateWG.Wait()
	got, err := store.GetTask(gated.ID)
	if err != nil {
		t.Fatalf("get gated: %v", err)
	}
	if got.SkipCount != 1 || got.Status != models.TaskStatusScheduled {
		t.Fatalf("want soft hold with 1 skip, got status=%s skips=%d", got.Status, got.SkipCount)
	}
	// First decline (skip_count was 0): retry ~30s out — in particular, in the
	// future, so the next tick does NOT re-run the command.
	now := time.Now().UTC()
	if got.ScheduledFor == nil || !got.ScheduledFor.After(now) || got.ScheduledFor.After(now.Add(2*time.Minute)) {
		t.Errorf("scheduled_for = %v, want ~30s after %v (first-decline backoff)", got.ScheduledFor, now)
	}

	// A task with an accumulated skip history backs off further: with 5 skips
	// recorded, the next decline schedules the retry 30s*2^5 = 16m out.
	if _, err := store.DB().Conn().ExecContext(ctx,
		"UPDATE tasks SET skip_count = 5, scheduled_for = $1 WHERE id = $2", past, gated.ID); err != nil {
		t.Fatalf("re-due gated: %v", err)
	}
	s.ProcessScheduledTasks()
	s.gateWG.Wait()
	got, err = store.GetTask(gated.ID)
	if err != nil {
		t.Fatalf("get gated: %v", err)
	}
	if got.SkipCount != 6 {
		t.Fatalf("skip_count = %d, want 6", got.SkipCount)
	}
	now = time.Now().UTC()
	if got.ScheduledFor == nil || got.ScheduledFor.Before(now.Add(10*time.Minute)) || got.ScheduledFor.After(now.Add(20*time.Minute)) {
		t.Errorf("scheduled_for = %v, want ~16m after %v (backoff must grow with skip_count)", got.ScheduledFor, now)
	}
}

// TestStopDrainsInFlightGates pins the bounded shutdown drain: a gate still
// evaluating when Stop is called must have its promote/skip settle write land
// before Stop returns (gates faster than gateDrainTimeout — the normal,
// lightweight kind), so shutdown no longer races the settle writes. Before
// the drain, Stop returned immediately and this test read the task while its
// gate goroutine was still sleeping.
func TestStopDrainsInFlightGates(t *testing.T) {
	s, store := newTestScheduler(t)
	ctx := context.Background()

	past := time.Now().UTC().Add(-1 * time.Hour)
	gated := &models.Task{
		ID: uuid.New(), Prompt: "gated", Status: models.TaskStatusScheduled, CreatedAt: time.Now().UTC(),
		ScheduledFor: &past,
		// Slow enough that an undrained Stop observes it mid-flight, fast
		// enough to settle well inside gateDrainTimeout.
		RunIf: &models.RunIf{Command: "sleep 0.3", ExitCodeIs: 0, TimeoutSeconds: 5, OnError: models.RunIfOnErrorRun},
	}
	if _, err := store.AddTaskWithContext(ctx, gated); err != nil {
		t.Fatalf("add gated: %v", err)
	}

	s.ProcessScheduledTasks() // dispatches the gate to its goroutine
	s.Stop()                  // must drain the in-flight evaluation

	got, err := store.GetTask(gated.ID)
	if err != nil {
		t.Fatalf("get gated: %v", err)
	}
	if got.Status != models.TaskStatusPending {
		t.Errorf("status after Stop = %s, want pending (the passing gate's settle write must land before Stop returns)", got.Status)
	}
}

// TestStopDoesNotRaceRunLoopGateDispatch pins Stop against the real Start/Stop
// lifecycle: Stop must wait for runLoop to exit BEFORE draining gateWG. A tick
// already inside the loop body when stop closes can still call gateWG.Add via
// tryDispatchGateEval, and an Add racing the drain's Wait is a sync.WaitGroup
// misuse — a panic in Stop's drain goroutine (no recover there) that kills the
// process mid-shutdown. The interleaving is what the race detector flags, so
// this test is load-bearing under -race: before the fix it reported the
// unsynchronized Add/Wait pair on every run where a gate was in flight at
// Stop. The gate dispatch is detected via a marker file the gate command
// writes — deliberately NOT via the scheduler's own mutexes, which would add
// the very happens-before edge the test must prove exists without them.
func TestStopDoesNotRaceRunLoopGateDispatch(t *testing.T) {
	s, store := newTestScheduler(t)
	s.tickInterval = 5 * time.Millisecond
	ctx := context.Background()

	marker := filepath.Join(t.TempDir(), "gate-dispatched")
	past := time.Now().UTC().Add(-time.Minute)
	gated := &models.Task{
		ID: uuid.New(), Prompt: "gated", Status: models.TaskStatusScheduled, CreatedAt: time.Now().UTC(),
		ScheduledFor: &past,
		// Long enough to still be in flight when Stop is called, short enough
		// to settle well inside gateDrainTimeout.
		RunIf: &models.RunIf{Command: "touch " + marker + "; sleep 0.5", ExitCodeIs: 0, TimeoutSeconds: 30},
	}
	if _, err := store.AddTaskWithContext(ctx, gated); err != nil {
		t.Fatalf("add gated: %v", err)
	}

	s.Start()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("runLoop never dispatched the gate")
		}
		time.Sleep(time.Millisecond)
	}
	s.Stop() // must first wait out runLoop, then drain the in-flight gate

	got, err := store.GetTask(gated.ID)
	if err != nil {
		t.Fatalf("get gated: %v", err)
	}
	if got.Status != models.TaskStatusPending {
		t.Errorf("status after Stop = %s, want pending (drain semantics must survive the loop-exit wait)", got.Status)
	}
}
