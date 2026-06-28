// Package scheduler provides the task scheduler: it promotes scheduled tasks to
// pending when due and runs RecoverExpiredLeases as the crash-safe backstop.
// Ported from moc's internal/scheduler. Task execution itself is handled by the
// in-process worker pool (internal/runner), which leases pending tasks.
package scheduler

import (
	"context"
	"log"
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

// ProcessScheduledTasks promotes due scheduled tasks to pending.
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
		taskIDs := make([]uuid.UUID, len(tasks))
		for i, task := range tasks {
			if task.Recurrence != "" {
				recurringCount++
			}
			taskIDs[i] = task.ID
		}
		log.Printf("Processing %d scheduled tasks (%d recurring)", len(tasks), recurringCount)

		promoted, err := s.storage.UpdateTasksStatusBatch(taskIDs, models.TaskStatusScheduled, models.TaskStatusPending)
		if err != nil {
			log.Printf("Error updating scheduled tasks batch: %v", err)
			successCount := 0
			for _, task := range tasks {
				n, err := s.storage.UpdateTasksStatusBatch([]uuid.UUID{task.ID}, models.TaskStatusScheduled, models.TaskStatusPending)
				if err != nil {
					log.Printf("Error updating task %s: %v", task.ID, err)
					continue
				}
				if n > 0 {
					log.Printf("Task %s is now pending", task.ID)
				}
				successCount++
			}
			if successCount == 0 {
				log.Printf("Failed to update any scheduled tasks in batch, aborting to prevent infinite loop")
				break
			}
		} else {
			log.Printf("Successfully promoted %d of %d scheduled tasks to pending", promoted, len(tasks))
		}

		if len(tasks) < batchSize {
			break
		}
	}
}
