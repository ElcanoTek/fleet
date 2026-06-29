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
}

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

// New creates a new Scheduler.
func New(store *storage.Storage, timezone string) *Scheduler {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		log.Printf("Warning: Invalid timezone '%s', defaulting to UTC: %v", timezone, err)
		loc = time.UTC
	}
	return &Scheduler{
		storage:  store,
		location: loc,
		stop:     make(chan struct{}),
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

	for {
		select {
		case <-ticker.C:
			// Recover per tick so a panic in task promotion or lease recovery
			// fails only that tick — it must never kill the loop or the process.
			func() {
				defer safe.Recover("scheduler.tick", nil)
				s.ProcessScheduledTasks()
				s.RecoverExpiredLeases()
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
	batchSize := 1000

	for {
		tasks, err := s.storage.GetScheduledTasks(now, batchSize)
		if err != nil {
			log.Printf("Error getting scheduled tasks: %v", err)
			return
		}
		if len(tasks) == 0 {
			return
		}

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
			successCount := 0
			for _, taskID := range promoteIDs {
				n, err := s.storage.UpdateTasksStatusBatch([]uuid.UUID{taskID}, models.TaskStatusScheduled, models.TaskStatusPending)
				if err != nil {
					log.Printf("Error updating task %s: %v", taskID, err)
					continue
				}
				if n > 0 {
					log.Printf("Task %s is now pending", taskID)
				}
				successCount++
			}
			if successCount == 0 {
				log.Printf("Failed to update any scheduled tasks in batch, aborting to prevent infinite loop")
				break
			}
		} else {
			log.Printf("Successfully promoted %d of %d scheduled tasks to pending", promoted, len(promoteIDs))
		}

		if len(tasks) < batchSize {
			break
		}
	}
}

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
