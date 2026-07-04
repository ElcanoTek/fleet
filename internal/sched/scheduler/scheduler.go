// Package scheduler provides the task scheduler: it promotes scheduled tasks to
// pending when due and runs RecoverExpiredLeases as the crash-safe backstop.
// Ported from moc's internal/scheduler. Task execution itself is handled by the
// in-process worker pool (internal/runner), which leases pending tasks.
package scheduler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/metrics"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// Scheduler manages scheduled and recurring tasks. Scheduling is intentionally
// POLL-based (the 30s runLoop ticker over a DB-backed queue, single-host), not an
// in-memory cron engine; recurrence timezone/DST math lives in storage/handlers
// via cron.ParseStandard(...).Next(now.In(location)).
type Scheduler struct {
	storage  *storage.Storage
	location *time.Location
	stop     chan struct{}

	// Automatic run-history retention (#252). retentionDays<=0 disables the daily
	// pruning sweep entirely; otherwise terminal runs older than retentionDays are
	// pruned daily at cleanupHour:00 UTC, always keeping keepPerTask runs per task.
	retentionDays int
	keepPerTask   int
	cleanupHour   int

	// Automatic log archival (#272). archiveAfterDays<=0 disables the daily
	// archival sweep (the conservative default); otherwise log payloads for
	// terminal tasks older than archiveAfterDays are compressed (optionally
	// encrypted) in place daily at cleanupHour:00 UTC. Reads stay transparent.
	archiveAfterDays int

	// Anti-starvation promotion (#230). starvationWindowMin<=0 disables it;
	// otherwise every runLoop tick promotes pending tasks that have waited longer
	// than this many minutes (and are still less urgent than the starvation
	// floor) up to that floor, so a sustained stream of higher-priority work can
	// never queue a low-priority task forever.
	starvationWindowMin int

	// Paused-task expiry (#510). pausedExpiryMin<=0 disables it (the default —
	// an ask-pause then waits forever); otherwise every runLoop tick fails
	// tasks that have sat in paused_awaiting_input longer than this, so an
	// unattended question can't strand a task indefinitely.
	pausedExpiryMin int

	// scheduledBatchSize is the page size ProcessScheduledTasks fetches due tasks
	// in. Defaults to defaultScheduledBatchSize; a field only so tests can inject
	// a small value and exercise the multi-batch / soft-hold paths cheaply.
	scheduledBatchSize int
}

// defaultScheduledBatchSize is the number of due tasks ProcessScheduledTasks
// promotes per DB round-trip.
const defaultScheduledBatchSize = 1000

// SetRetention configures the automatic daily run-history pruning sweep (#252).
// Call before Start. retentionDays<=0 leaves pruning OFF (the default). hour is
// clamped to 0–23.
func (s *Scheduler) SetRetention(retentionDays, keepPerTask, hour int) {
	s.retentionDays = retentionDays
	s.keepPerTask = keepPerTask
	if hour < 0 || hour > 23 {
		hour = 4
	}
	s.cleanupHour = hour
}

// SetLogArchival configures the automatic daily log-archival sweep (#272). Call
// before Start. archiveAfterDays<=0 leaves archival OFF (the conservative
// default). The sweep runs on the same daily timer as the retention sweep
// (cleanupHour). Archival is purely a storage optimization: reads inflate
// archived payloads transparently, so it never changes what a caller sees.
func (s *Scheduler) SetLogArchival(archiveAfterDays int) {
	s.archiveAfterDays = archiveAfterDays
}

// SetStarvationWindow configures the anti-starvation promotion sweep (#230).
// Call before Start. windowMinutes<=0 leaves promotion OFF; otherwise a pending
// task that has waited longer than this is promoted to the starvation floor so
// it can't be starved indefinitely by a stream of higher-priority work.
func (s *Scheduler) SetStarvationWindow(windowMinutes int) {
	s.starvationWindowMin = windowMinutes
}

// SetPausedExpiry configures the paused-task expiry sweep (#510). Call before
// Start. windowMinutes<=0 leaves expiry OFF (a paused task waits forever);
// otherwise a task awaiting input longer than this is failed with a terminal
// error so it doesn't strand.
func (s *Scheduler) SetPausedExpiry(windowMinutes int) {
	s.pausedExpiryMin = windowMinutes
}

// New creates a new Scheduler.
func New(store *storage.Storage, timezone string) *Scheduler {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		log.Printf("Warning: Invalid timezone '%s', defaulting to UTC: %v", timezone, err)
		loc = time.UTC
	}
	return &Scheduler{
		storage:            store,
		location:           loc,
		stop:               make(chan struct{}),
		scheduledBatchSize: defaultScheduledBatchSize,
	}
}

// Start starts the scheduler.
func (s *Scheduler) Start() {
	log.Println("Starting scheduler...")
	go s.runLoop()
}

// Stop stops the scheduler.
func (s *Scheduler) Stop() { close(s.stop) }

func (s *Scheduler) runLoop() {
	defer safe.Recover("scheduler.runLoop", nil)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	// Daily maintenance sweep: a timer that fires at the next cleanupHour:00 UTC,
	// then re-arms every 24h. Runs the retention prune (#252) and the log-archival
	// pass (#272). Disabled (nil channel — never selected) when BOTH are off.
	var cleanupC <-chan time.Time
	var cleanupTimer *time.Timer
	if s.retentionDays > 0 || s.archiveAfterDays > 0 {
		cleanupTimer = time.NewTimer(durationUntilHour(time.Now().UTC(), s.cleanupHour))
		defer cleanupTimer.Stop()
		cleanupC = cleanupTimer.C
	}
	if s.retentionDays > 0 {
		log.Printf("scheduler: run-history retention ON (retention=%dd, keep=%d/task, sweep daily at %02d:00 UTC)",
			s.retentionDays, s.keepPerTask, s.cleanupHour)
	}
	if s.archiveAfterDays > 0 {
		log.Printf("scheduler: log archival ON (archive after %dd, sweep daily at %02d:00 UTC)",
			s.archiveAfterDays, s.cleanupHour)
	}
	if s.starvationWindowMin > 0 {
		log.Printf("scheduler: anti-starvation promotion ON (promote pending tasks waiting > %dm to priority %d)",
			s.starvationWindowMin, models.StarvationFloorPriority)
	}
	if s.pausedExpiryMin > 0 {
		log.Printf("scheduler: paused-task expiry ON (fail tasks awaiting input > %dm)", s.pausedExpiryMin)
	}

	for {
		select {
		case <-ticker.C:
			// Recover per tick so a panic in task promotion or lease recovery
			// fails only that tick — it must never kill the loop or the process.
			func() {
				defer safe.Recover("scheduler.tick", nil)
				s.ProcessScheduledTasks()
				s.RecoverExpiredLeases()
				s.runStarvationPromotion()
				s.runPausedExpiry()
			}()
		case <-cleanupC:
			func() {
				defer safe.Recover("scheduler.cleanup", nil)
				s.runCleanup()
				s.runLogArchival()
			}()
			cleanupTimer.Reset(24 * time.Hour)
		case <-s.stop:
			return
		}
	}
}

// runStarvationPromotion performs one anti-starvation sweep (#230): it promotes
// pending tasks that have waited past the window up to the starvation floor.
// No-op when disabled. Logs only when it actually promotes something, so a quiet
// queue stays quiet in the logs; a failure is logged but never fatal — the next
// tick retries.
func (s *Scheduler) runStarvationPromotion() {
	if s.starvationWindowMin <= 0 {
		return
	}
	n, err := s.storage.PromoteStarvedTasks(context.Background(), s.starvationWindowMin)
	if err != nil {
		log.Printf("scheduler: starvation promotion failed: %v", err)
		return
	}
	if n > 0 {
		log.Printf("scheduler: promoted %d starving task(s) to priority %d (waited > %dm)",
			n, models.StarvationFloorPriority, s.starvationWindowMin)
	}
}

// runPausedExpiry performs one paused-task expiry sweep (#510): it fails tasks
// that have awaited input past the window. No-op when disabled. Logs only when
// it expires something; a failure is logged but never fatal — the next tick
// retries.
func (s *Scheduler) runPausedExpiry() {
	if s.pausedExpiryMin <= 0 {
		return
	}
	n, err := s.storage.ExpirePausedTasks(context.Background(), s.pausedExpiryMin)
	if err != nil {
		log.Printf("scheduler: paused-task expiry failed: %v", err)
		return
	}
	if n > 0 {
		log.Printf("scheduler: expired %d paused task(s) awaiting input > %dm", n, s.pausedExpiryMin)
	}
}

// runCleanup performs one retention sweep, logging + counting what it pruned.
func (s *Scheduler) runCleanup() {
	n, err := s.storage.CleanupOldRuns(context.Background(), s.retentionDays, s.keepPerTask)
	if err != nil {
		log.Printf("scheduler: run-history cleanup failed: %v", err)
		return
	}
	if n > 0 {
		log.Printf("scheduler: pruned %d old task run(s) (retention=%dd, keep=%d/task)", n, s.retentionDays, s.keepPerTask)
		metrics.RecordRunsPruned(n)
	}
}

// runLogArchival performs one log-archival pass (#272), compressing (optionally
// encrypting) terminal-task log payloads older than archiveAfterDays in place.
// No-op when archival is off. Failures are logged and counted but never fatal —
// the next daily sweep retries any rows it could not archive.
func (s *Scheduler) runLogArchival() {
	if s.archiveAfterDays <= 0 {
		return
	}
	n, bytesSaved, err := s.storage.ArchiveOldLogs(context.Background(), s.archiveAfterDays)
	if err != nil {
		log.Printf("scheduler: log archival failed: %v", err)
		metrics.RecordLogsArchived("error", 0, 0)
		return
	}
	if n > 0 {
		log.Printf("scheduler: archived %d task log(s), saved %d bytes (archive after %dd)", n, bytesSaved, s.archiveAfterDays)
		metrics.RecordLogsArchived("ok", n, bytesSaved)
	}
}

// durationUntilHour returns the time from `now` until the next occurrence of
// hour:00 (in now's location). If now is exactly at the top of that hour it
// returns ~24h (next day) rather than 0, so the first sweep doesn't fire instantly.
func durationUntilHour(now time.Time, hour int) time.Duration {
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location())
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next.Sub(now)
}

// RecoverExpiredLeases re-queues tasks whose lease expired (crash recovery).
func (s *Scheduler) RecoverExpiredLeases() {
	count, err := s.storage.RecoverExpiredLeases()
	if err != nil {
		log.Printf("Error recovering expired leases: %v", err)
		return
	}
	if count > 0 {
		log.Printf("Recovered %d tasks with expired leases", count)
	}
}

// ProcessScheduledTasks promotes due scheduled tasks to pending. Tasks that
// carry a pre-run shell gate (#269) are evaluated first; only those whose check
// passes (or whose on_error policy is "run") are promoted. Failing checks skip
// the task (scheduled_for advances, skip_count increments) via handleSkip.
func (s *Scheduler) ProcessScheduledTasks() {
	now := time.Now().In(s.location)
	batchSize := s.scheduledBatchSize
	if batchSize <= 0 {
		batchSize = defaultScheduledBatchSize
	}

	// A task leaves the "scheduled + due" set only when it is promoted or its
	// scheduled_for is advanced. Tasks that DON'T leave it — a soft-held one-shot
	// whose run_if declined, a recurring task whose ComputeNextRun errored, a row
	// whose promote UPDATE failed — are re-returned by every plain-LIMIT re-fetch
	// (same `now`, same ordering), so a full batch of them used to loop forever,
	// hanging the scheduler goroutine and with it lease recovery and starvation
	// promotion (#566). Walk the due set with a KEYSET cursor over the total
	// order (scheduled_for, id) instead: the cursor advances past every fetched
	// row — held or not — so each row is handled at most once per tick, each pass
	// fetches strictly-later rows, and termination is structural (the due set is
	// finite and `now` is fixed). A held prefix can't mask due work behind it,
	// and the per-tick cost stays linear in the due-set size.
	var (
		cursorScheduledFor time.Time
		cursorID           uuid.UUID
		rowsThisTick       int
	)

	for {
		// Defensive valve: bounds the per-tick work if an operator somehow
		// accumulates an enormous due set. The next tick (30s) starts over and —
		// promotions having emptied the front of the due set — reaches the rest.
		if rowsThisTick >= maxScheduledRowsPerTick {
			log.Printf("ProcessScheduledTasks: handled %d due rows this tick, deferring the rest to the next tick", rowsThisTick)
			return
		}

		tasks, err := s.storage.GetScheduledTasks(now, cursorScheduledFor, cursorID, batchSize)
		if err != nil {
			log.Printf("Error getting scheduled tasks: %v", err)
			return
		}
		if len(tasks) == 0 {
			return
		}
		rowsThisTick += len(tasks)
		// Every fetched row satisfies scheduled_for IS NOT NULL (the query requires
		// it), so the cursor always advances to the last row of the page.
		last := tasks[len(tasks)-1]
		if last.ScheduledFor != nil {
			cursorScheduledFor = *last.ScheduledFor
		}
		cursorID = last.ID

		recurringCount := 0
		promoteIDs := make([]uuid.UUID, 0, len(tasks))
		var toSkip []taskSkip
		for _, task := range tasks {
			if task.Recurrence != "" {
				recurringCount++
			}
			if task.RunIf == nil {
				promoteIDs = append(promoteIDs, task.ID)
				continue
			}
			ok, reason, err := s.evalRunIf(task)
			if err != nil {
				if task.RunIf.EffectiveOnError() == models.RunIfOnErrorSkip {
					toSkip = append(toSkip, taskSkip{task: task, reason: "check_error: " + err.Error()})
				} else {
					promoteIDs = append(promoteIDs, task.ID)
				}
				continue
			}
			if ok {
				promoteIDs = append(promoteIDs, task.ID)
			} else {
				toSkip = append(toSkip, taskSkip{task: task, reason: reason})
			}
		}
		log.Printf("Processing %d scheduled tasks (%d recurring, %d to skip)", len(tasks), recurringCount, len(toSkip))

		for _, sk := range toSkip {
			s.handleSkip(sk.task, sk.reason)
		}

		promoted, err := s.storage.UpdateTasksStatusBatch(promoteIDs, models.TaskStatusScheduled, models.TaskStatusPending)
		if err != nil {
			log.Printf("Error updating scheduled tasks batch: %v", err)
			// Fall back to per-task promotion so one bad row can't strand the batch.
			// A row that still fails is already behind the cursor, so a persistent
			// DB error can't re-loop it this tick.
			for _, taskID := range promoteIDs {
				n, perr := s.storage.UpdateTasksStatusBatch([]uuid.UUID{taskID}, models.TaskStatusScheduled, models.TaskStatusPending)
				if perr != nil {
					log.Printf("Error updating task %s: %v", taskID, perr)
					continue
				}
				if n > 0 {
					log.Printf("Task %s is now pending", taskID)
				}
			}
		} else {
			log.Printf("Successfully promoted %d of %d scheduled tasks to pending", promoted, len(promoteIDs))
		}

		// A short page means the cursor walked off the end of the due set.
		if len(tasks) < batchSize {
			return
		}
	}
}

// maxScheduledRowsPerTick bounds how many due rows one ProcessScheduledTasks
// tick will handle. Far above any plausible simultaneous due set; purely a
// defensive valve for the #566 cursor walk.
const maxScheduledRowsPerTick = 100_000

// taskSkip pairs a task with the human-readable reason its pre-run gate
// declined it (#269). The reason is recorded on the task row (last_skip_reason)
// and logged.
type taskSkip struct {
	task   *models.Task
	reason string
}

// evalRunIf evaluates a task's pre-run shell gate on the host (#269). The check
// runs via `sh -c` as the fleet process user with a restricted PATH (no sudo,
// no package managers). The task should run iff the command exits with
// ExitCodeIs. shouldRun=true means promote; shouldRun=false means skip (the
// returned reason is recorded on the task). A non-nil err means the check
// ITSELF errored (timeout/crash/signal) — the caller applies the task's
// on_error policy (run anyway vs. skip).
//
// SECURITY: the check is a host-side shell invocation, NOT a sandboxed agent
// tool call. By design (it is a lightweight gate, not an agent capability), so
// a misconfigured check cannot burn a model budget or touch MCP credentials —
// but it DOES carry the host-user privileges of the fleet process. Operators
// must treat run_if commands as trusted, exactly like the fleet binary itself.
// stdout/stderr are captured only for the skip-reason log; they do not enter
// the task result or the model context.
func (s *Scheduler) evalRunIf(task *models.Task) (shouldRun bool, reason string, err error) {
	timeout := time.Duration(task.RunIf.EffectiveTimeoutSeconds()) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", task.RunIf.Command) //nolint:gosec // G204: run_if is an operator-trusted host-side gate by design (#269); see RunIf doc.
	// Restricted PATH: no sudo, no package managers. HOME=/tmp so a command
	// that reads $HOME (e.g. git -C) doesn't fail on a missing home dir, and so
	// a stray write doesn't pollute the fleet process's real home.
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/tmp"}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return false, "check timed out", ctx.Err()
	}
	want := task.RunIf.ExitCodeIs
	if runErr != nil {
		exitCode := -1
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		if exitCode == want {
			return true, "", nil
		}
		return false, fmt.Sprintf("exit %d (want %d): %s", exitCode, want, stderr.String()), nil
	}
	// exit 0
	if want == 0 {
		return true, "", nil
	}
	return false, fmt.Sprintf("exit 0 (want %d)", want), nil
}

// handleSkip records a pre-run-gate skip on a task (#269) and advances its
// scheduled_for to the next cron tick. For a non-recurring task there is no
// next tick: the skip is recorded (skip_count++, last_skip_at/reason stamped)
// without advancing scheduled_for, so the task stays due and will be re-evaluated
// on the next tick (a one-shot skip is effectively a soft hold, not a cancel —
// see the issue's non-goals). Failures are logged; they never abort the tick.
func (s *Scheduler) handleSkip(task *models.Task, reason string) {
	class := "check_failed"
	var nextRun time.Time
	if task.Recurrence != "" {
		computed, err := s.storage.ComputeNextRun(task)
		if err != nil {
			log.Printf("Error computing next run for skipped task %s: %v", task.ID, err)
		} else {
			nextRun = computed
		}
	}
	ctx := context.Background()
	if _, err := s.storage.RecordSkip(ctx, task.ID, reason, nextRun); err != nil {
		log.Printf("Error recording skip for task %s: %v", task.ID, err)
		return
	}
	// Distinguish a check_error (the gate timed out / crashed) from a clean
	// check_failed (the command exited with an unexpected code) for the metric
	// label, so dashboards can separate "gate is misconfigured" from "gate is
	// declining work".
	if strings.HasPrefix(reason, "check_error:") {
		class = "check_error"
	}
	metrics.RecordTaskSkipped(class)
	nextStr := "none (one-shot)"
	if !nextRun.IsZero() {
		nextStr = nextRun.Format(time.RFC3339)
	}
	log.Printf(`{"event":"task_skipped","task_id":"%s","reason":"%s","next_run_at":"%s"}`,
		task.ID, reason, nextStr)
}
