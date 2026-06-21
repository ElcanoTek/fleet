// Package scheduler provides the task scheduler: it promotes scheduled tasks to
// pending when due and runs RecoverExpiredLeases as the crash-safe backstop.
// Ported from moc's internal/scheduler. Task execution itself is handled by the
// in-process worker pool (internal/runner), which leases pending tasks.
package scheduler

import (
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// Scheduler manages scheduled and recurring tasks.
type Scheduler struct {
	storage  *storage.Storage
	cron     *cron.Cron
	location *time.Location
	stop     chan struct{}
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
		cron:     cron.New(cron.WithLocation(loc)),
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
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.ProcessScheduledTasks()
			s.RecoverExpiredLeases()
		case <-s.stop:
			return
		}
	}
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
