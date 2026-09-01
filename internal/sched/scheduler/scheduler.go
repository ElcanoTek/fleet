// Package scheduler provides the task scheduler: it promotes scheduled tasks to
// pending when due and runs RecoverExpiredLeases as the crash-safe backstop.
// Ported from moc's internal/scheduler. Task execution itself is handled by the
// in-process worker pool (internal/runner), which leases pending tasks.
package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/metrics"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/sched/db"
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

	// loopDone is closed by runLoop on return; loopStarted records whether
	// Start actually launched it. Stop must wait for the loop to exit before
	// draining gateWG: a tick already inside the loop body when stop closes
	// can still dispatch gate evaluations (gateWG.Add), and sync.WaitGroup
	// forbids an Add that races a Wait — that misuse panics, and a panic in
	// Stop's drain goroutine has no recover and would kill the process
	// mid-shutdown. Only after runLoop returns is the dispatch side quiescent.
	loopDone    chan struct{}
	loopStarted bool

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

	// tickInterval is the runLoop ticker period. Defaults to
	// defaultTickInterval; a field only so tests can shrink it and drive real
	// Start/Stop lifecycles without waiting out the production cadence.
	tickInterval time.Duration

	// Bounded, asynchronous run_if evaluation (#269 hardening). A gate is a
	// host-side shell command with a timeout of up to 300s; evaluating it
	// inline used to serialize the whole tick — one hung gate delayed ALL
	// scheduling and lease recovery by its full timeout, and the 30s ticker
	// drops ticks while the body blocks. Gates are instead dispatched to
	// goroutines: gateSlots caps how many run concurrently (a full pool defers
	// the task to a later tick — it stays scheduled and due), gateInFlight
	// (under gateMu) prevents the next tick from double-dispatching a task
	// whose gate is still running, and gateWG lets Stop (bounded by
	// gateDrainTimeout) and tests wait for outstanding evaluations to settle.
	gateSlots    chan struct{}
	gateMu       sync.Mutex
	gateInFlight map[uuid.UUID]struct{}
	gateWG       sync.WaitGroup
}

// maxConcurrentGateEvals bounds how many run_if gate commands may execute at
// once. Small on purpose: gates are meant to be lightweight checks, and the
// bound is what keeps N hung gates from consuming unbounded host processes —
// the tick itself never waits on any of them.
const maxConcurrentGateEvals = 4

// defaultScheduledBatchSize is the number of due tasks ProcessScheduledTasks
// promotes per DB round-trip.
const defaultScheduledBatchSize = 1000

// defaultTickInterval is how often runLoop wakes to promote due tasks and run
// the cheap DB sweeps.
const defaultTickInterval = 30 * time.Second

// maxRunIfStderrBytes bounds diagnostics retained from an admin-authored
// pre-run gate. The command itself is time-bounded, but without a byte bound a
// noisy gate could still grow the scheduler process's heap until the timeout.
const maxRunIfStderrBytes = 8 << 10

const runIfStderrTruncated = "\n…[stderr truncated]"

// runIfWaitDelay bounds how long cmd.Run may keep waiting on the stderr pipe
// after the gate's process exits or its timeout fires. Without it a grandchild
// that inherited the pipe and escaped the process-group kill (setsid) would
// block the sequential scheduler tick indefinitely.
const runIfWaitDelay = 10 * time.Second

type cappedRunIfStderr struct {
	buf       bytes.Buffer
	truncated bool
}

func (w *cappedRunIfStderr) Write(p []byte) (int, error) {
	written := len(p)
	remaining := maxRunIfStderrBytes - w.buf.Len()
	if remaining < len(p) {
		w.truncated = true
	}
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = w.buf.Write(p)
	}
	// Report the whole input consumed so os/exec keeps draining the pipe after
	// the retained prefix reaches its cap.
	return written, nil
}

func (w *cappedRunIfStderr) String() string {
	if w.truncated {
		return w.buf.String() + runIfStderrTruncated
	}
	return w.buf.String()
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

// SetTickInterval configures the runLoop cadence (FLEET_SCHED_TICK_SECONDS).
// Call before Start. seconds<=0 keeps the 30s default. The tick bounds the
// worst-case latency between a task becoming due and a worker claiming it, so
// dev boxes and conformance rigs driving synchronous callers (the A2A blocking
// unary wait, the official TCK) shrink it; production deployments should not —
// every tick is a full due-task scan plus lease-recovery pass against the DB.
func (s *Scheduler) SetTickInterval(seconds int) {
	if seconds > 0 {
		s.tickInterval = time.Duration(seconds) * time.Second
	}
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
		loopDone:           make(chan struct{}),
		scheduledBatchSize: defaultScheduledBatchSize,
		tickInterval:       defaultTickInterval,
		gateSlots:          make(chan struct{}, maxConcurrentGateEvals),
		gateInFlight:       make(map[uuid.UUID]struct{}),
	}
}

// Start starts the scheduler.
func (s *Scheduler) Start() {
	log.Println("Starting scheduler...")
	s.loopStarted = true
	go s.runLoop()
}

// gateDrainTimeout bounds how long Stop waits for in-flight run_if gate
// evaluations to settle. Gates are meant to be lightweight checks, so the
// common case drains in well under this; a slower gate (they may legitimately
// run up to 300s) must never hold shutdown for its full runtime, so the wait
// is a bound, not a guarantee.
const gateDrainTimeout = 5 * time.Second

// Stop stops the scheduler, waiting up to gateDrainTimeout for runLoop to
// exit and for in-flight run_if gate evaluations to settle their promote/skip
// writes. The loop-exit wait comes FIRST, and not just for tidiness: a tick
// already inside the loop body when stop closes can still dispatch gates
// (gateWG.Add), and calling gateWG.Wait concurrently with those Adds is a
// sync.WaitGroup misuse panic — in a goroutine with no recover, so it would
// kill the process mid-shutdown and skip main's remaining deferred cleanup.
// A gate still running at the deadline is abandoned to finish on its own: its
// settle write is conditional (fromStatus=scheduled), so it either lands as
// it would have or fails harmlessly, and a task left `scheduled` is simply
// re-evaluated after restart. The bounded wait exists only to close that
// shutdown race in the common case, never to let a slow gate extend shutdown.
func (s *Scheduler) Stop() {
	close(s.stop)
	settled := make(chan struct{})
	go func() {
		// Tests may drive ticks directly without Start; only a started loop
		// has a runLoop to wait out (and only runLoop dispatches concurrently
		// with Stop — direct ProcessScheduledTasks callers are sequential).
		if s.loopStarted {
			<-s.loopDone
		}
		s.gateWG.Wait()
		close(settled)
	}()
	select {
	case <-settled:
	case <-time.After(gateDrainTimeout):
		log.Printf("scheduler: stopped with run_if gate evaluations still in flight; their tasks stay scheduled and are re-evaluated after restart")
	}
}

func (s *Scheduler) runLoop() {
	defer close(s.loopDone)
	defer safe.Recover("scheduler.runLoop", nil)
	ticker := time.NewTicker(s.tickInterval)
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
			//
			// Order matters: the five cheap DB sweeps run BEFORE task promotion,
			// with lease recovery first — it is the crash-recovery backstop, and
			// promotion is the one tick phase whose cost scales with operator
			// config (run_if gates; bounded + async, but defense in depth says a
			// regression there must not be able to starve recovery of crashed
			// workers' tasks).
			func() {
				defer safe.Recover("scheduler.tick", nil)
				s.RecoverExpiredLeases()
				s.runStarvationPromotion()
				s.runPausedExpiry()
				s.runWakeSweep()
				// AFTER the wake sweep, deliberately: a row that is merely due
				// gets woken on this same tick and is therefore never a
				// candidate here. Only rows the wake sweep could not reach
				// survive to be expired.
				s.runStrandedWakeExpiry()
				s.runRecurrenceReconciliation()
				s.ProcessScheduledTasks()
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

// runStrandedWakeExpiry fails tasks parked in paused_awaiting_wake that no wake
// can ever reach. Unlike runPausedExpiry this has NO disable knob and is always
// on: an unreachable parked row is a broken row, not an operator policy choice,
// and the alternative is a task that waits forever with no terminal record.
// Logs only when it expires something; a failure is logged but never fatal —
// the next tick retries.
func (s *Scheduler) runStrandedWakeExpiry() {
	n, err := s.storage.ExpireStrandedWakeTasks(context.Background(), db.StrandedWakeGrace)
	if err != nil {
		log.Printf("scheduler: stranded-wake expiry failed: %v", err)
		return
	}
	if n > 0 {
		log.Printf("scheduler: expired %d task(s) parked awaiting a wake that can no longer arrive (> %s past the deadline)", n, db.StrandedWakeGrace)
	}
}

// runWakeSweep performs one self-wake sweep (docs/SELF-WAKE.md): it re-queues
// parked tasks whose wake deadline has passed. Always on — a wake is core
// task lifecycle, not a tunable policy — and data-driven: with no parked
// tasks the sweep is one cheap indexed query. Logs only when it wakes
// something; a failure is logged but never fatal — the next tick retries.
func (s *Scheduler) runWakeSweep() {
	n, err := s.storage.WakeDueTasks(context.Background())
	if err != nil {
		log.Printf("scheduler: wake sweep failed: %v", err)
		return
	}
	if n > 0 {
		log.Printf("scheduler: woke %d task(s) whose wake deadline passed", n)
	}
}

// runRecurrenceReconciliation performs one recurrence-chain repair sweep
// (#1116): it re-spawns the successor of any terminal recurring occurrence
// whose post-completion spawn failed or was lost to a crash — the failure mode
// that used to end a schedule forever with nothing but a log line. Always on —
// like the wake sweep, chain continuity is core task lifecycle, not a tunable
// policy — and data-driven: with no orphaned chains it is one cheap indexed
// probe (idx_tasks_recurrence_unspawned). Logs + counts only when it repairs
// something; a failure is logged but never fatal — the next tick retries.
func (s *Scheduler) runRecurrenceReconciliation() {
	n, err := s.storage.ReconcileRecurrences(context.Background())
	if err != nil {
		log.Printf("scheduler: recurrence reconciliation failed: %v", err)
		return
	}
	if n > 0 {
		log.Printf("scheduler: reconciled %d recurring schedule(s) whose post-completion spawn never landed", n)
		metrics.RecordRecurrencesReconciled(n)
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
// Tasks already past their retry budget are dead-lettered by the storage layer
// instead (#1116, the crash-loop bound) and are not in the count logged here.
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

// ProcessScheduledTasks promotes due scheduled tasks to pending. Ungated tasks
// are promoted in batch, synchronously. Tasks that carry a pre-run shell gate
// (#269) are handed to the bounded async evaluation pool and settled there —
// promoted when the check passes (or when it errors under on_error:"run"),
// skipped via handleSkip otherwise — so this tick never blocks on a gate.
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

		recurringCount, gatedCount, deferredGates := 0, 0, 0
		promoteIDs := make([]uuid.UUID, 0, len(tasks))
		for _, task := range tasks {
			if task.Recurrence != "" {
				recurringCount++
			}
			if task.RunIf == nil {
				promoteIDs = append(promoteIDs, task.ID)
				continue
			}
			// Gated tasks are settled ASYNCHRONOUSLY (promote or skip happens in
			// the eval goroutine) so a slow or hung gate can never stall this
			// tick. A task that can't be dispatched — its gate is still running
			// from an earlier tick, or the pool is full — is simply left in the
			// due set: the cursor has already advanced past it for THIS tick
			// (the #566 termination argument is unchanged), and the next tick
			// re-fetches it.
			gatedCount++
			if !s.tryDispatchGateEval(task) {
				deferredGates++
			}
		}
		log.Printf("Processing %d scheduled tasks (%d recurring, %d gated dispatched async, %d gated deferred)",
			len(tasks), recurringCount, gatedCount-deferredGates, deferredGates)

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

// tryDispatchGateEval hands a gated task to the async evaluation pool. It
// returns false — leaving the task scheduled and due, to be retried next tick —
// when the task's gate is already in flight from an earlier tick or the pool
// is at maxConcurrentGateEvals. The in-flight mark is taken under gateMu
// BEFORE the goroutine starts so two ticks can never race one task into two
// evaluations.
func (s *Scheduler) tryDispatchGateEval(task *models.Task) bool {
	s.gateMu.Lock()
	if _, busy := s.gateInFlight[task.ID]; busy {
		s.gateMu.Unlock()
		return false
	}
	select {
	case s.gateSlots <- struct{}{}:
	default:
		s.gateMu.Unlock()
		return false
	}
	s.gateInFlight[task.ID] = struct{}{}
	s.gateMu.Unlock()

	s.gateWG.Add(1)
	go s.evalAndSettleGate(task)
	return true
}

// evalAndSettleGate runs one gated task's run_if check and settles the task:
// promote to pending on pass (or on a check error under on_error:"run"), skip
// via handleSkip otherwise. Runs on its own goroutine; both settle paths
// re-check inside the DB write that the row is still `scheduled` AND still
// carries the scheduled_for this evaluation was dispatched against, so a task
// cancelled, edited, or rescheduled while its gate ran is never resurrected
// and never has an operator's fresh scheduled_for clobbered by the stale
// verdict.
func (s *Scheduler) evalAndSettleGate(task *models.Task) {
	defer s.gateWG.Done()
	defer func() {
		<-s.gateSlots
		s.gateMu.Lock()
		delete(s.gateInFlight, task.ID)
		s.gateMu.Unlock()
	}()
	defer safe.Recover("scheduler.gateEval", nil)

	ok, reason, err := s.evalRunIf(task)
	switch {
	case err != nil && task.RunIf.EffectiveOnError() == models.RunIfOnErrorSkip:
		s.handleSkip(task, "check_error: "+err.Error())
	case err != nil || ok:
		// Pass, or a check error under the default on_error:"run" policy.
		s.promoteGatedTask(task)
	default:
		s.handleSkip(task, reason)
	}
}

// promoteGatedTask promotes one gate-passing task from scheduled to pending.
// The conditional UPDATE (still `scheduled`, scheduled_for unchanged since
// dispatch) makes a concurrent cancel win exactly like the batch promotion
// path for ungated tasks — and, because gates settle asynchronously, also
// makes a concurrent edit/reschedule win: a task postponed while its gate ran
// must not run on the stale verdict (the next due tick re-evaluates it).
func (s *Scheduler) promoteGatedTask(task *models.Task) {
	n, err := s.storage.SettleGatedTask(task.ID, task.ScheduledFor, models.TaskStatusPending)
	if err != nil {
		log.Printf("Error promoting gated task %s: %v", task.ID, err)
		return
	}
	if n > 0 {
		log.Printf("Task %s passed its run_if gate and is now pending", task.ID)
	} else {
		log.Printf("Task %s changed while its run_if gate ran — stale pass verdict discarded", task.ID)
	}
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
	var stderr cappedRunIfStderr
	cmd.Stderr = &stderr
	// Run the gate as its own process-group leader and SIGKILL the WHOLE group
	// on cancel/timeout: CommandContext's default kill signals only the direct
	// sh, so a backgrounded grandchild would survive the timeout while holding
	// the stderr pipe open — and the pipe copier would block cmd.Run (and this
	// scheduler tick: lease recovery, the wake sweep) indefinitely. Same
	// invariant as the host sandbox's bash path (internal/sandbox/host.go).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return os.ErrProcessDone // group-killed above; nothing more to signal
	}
	// WaitDelay is the backstop for a grandchild that escaped the group (e.g.
	// setsid): it force-closes the stderr pipe after the gate exits so cmd.Run
	// honors the documented [1,300]s ceiling instead of blocking on the pipe.
	cmd.WaitDelay = runIfWaitDelay

	runErr := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return false, "check timed out", ctx.Err()
	}
	// ErrWaitDelay means the gate itself exited 0 and only the force-closed
	// pipe was abnormal (a lingering grandchild held it) — the recorded exit
	// status is authoritative, so treat it as a clean exit 0.
	if errors.Is(runErr, exec.ErrWaitDelay) {
		runErr = nil
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
// scheduled_for to the next cron tick. A non-recurring task has no next tick:
// the skip is recorded (skip_count++, last_skip_at/reason stamped) and
// scheduled_for advances by an exponential backoff on skip_count instead (a
// one-shot skip is a soft hold that keeps retrying, not a cancel — see the
// issue's non-goals — but without backoff a declined gate re-ran its host
// command every 30s tick forever). The same backoff covers a recurring task
// whose ComputeNextRun errored. Failures are logged; they never abort the tick.
func (s *Scheduler) handleSkip(task *models.Task, reason string) {
	class := "check_failed"
	var nextRun time.Time
	if task.Recurrence != "" {
		computed, err := s.storage.ComputeNextRun(task)
		switch {
		case err != nil:
			log.Printf("Error computing next run for skipped task %s: %v", task.ID, err)
		case task.RecurrenceUntil != nil && computed.After(*task.RecurrenceUntil):
			// The recurrence's end date falls before the next cron tick: there is
			// no future occurrence to advance to, so cancel the row instead of
			// leaving it due (it would otherwise re-skip on every tick forever).
			// Settled conditionally like the other verdict writes: an edit or
			// reschedule that landed while the gate ran (it may have changed the
			// recurrence or its end date) invalidates this cancel too.
			n, cerr := s.storage.SettleGatedTask(task.ID, task.ScheduledFor, models.TaskStatusCancelled)
			switch {
			case cerr != nil:
				log.Printf("Error ending recurrence for skipped task %s: %v", task.ID, cerr)
			case n > 0:
				log.Printf("Recurrence for task %s ended at skip: next tick %s is past recurrence_until %s",
					task.ID, computed.Format(time.RFC3339), task.RecurrenceUntil.Format(time.RFC3339))
			default:
				log.Printf("Task %s changed while its run_if gate ran — stale end-of-recurrence verdict discarded", task.ID)
			}
			return
		default:
			nextRun = computed
		}
	}
	// No future cron tick to advance to (one-shot, or ComputeNextRun errored):
	// back the retry off exponentially on the skips already recorded, so a
	// persistently-declining gate settles at one host command per
	// gateSkipBackoffCap instead of one per tick.
	if nextRun.IsZero() {
		nextRun = time.Now().UTC().Add(gateSkipBackoff(task.SkipCount))
	}
	ctx := context.Background()
	_, recorded, err := s.storage.RecordSkip(ctx, task.ID, reason, nextRun, task.ScheduledFor)
	if err != nil {
		log.Printf("Error recording skip for task %s: %v", task.ID, err)
		return
	}
	if !recorded {
		// The task was cancelled, claimed, edited, or rescheduled while the
		// gate ran; nothing was written, so don't count or log a skip that
		// never happened — the next due tick re-evaluates the current row.
		log.Printf("Task %s changed while its run_if gate ran — stale skip verdict discarded", task.ID)
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
	nextStr := nextRun.Format(time.RFC3339)
	// The reason carries raw gate stderr, so the event is json.Marshal'ed —
	// interpolated unescaped, a newline or quote in it would split or forge
	// log records. The log line clamps the reason to a short prefix; the full
	// capped text is already persisted to last_skip_reason by RecordSkip above.
	event, err := json.Marshal(struct {
		Event     string `json:"event"`
		TaskID    string `json:"task_id"`
		Reason    string `json:"reason"`
		NextRunAt string `json:"next_run_at"`
	}{"task_skipped", task.ID.String(), clampSkipReason(reason), nextStr})
	if err != nil {
		log.Printf("Error marshaling skip event for task %s: %v", task.ID, err)
		return
	}
	log.Printf("%s", event)
}

// gateSkipBackoffBase / gateSkipBackoffCap bound the retry cadence of a
// declined gate with no future cron tick to advance to (a one-shot's soft
// hold, or a recurring task whose ComputeNextRun failed): 30s, 1m, 2m, …,
// doubling per recorded skip up to the cap. The base matches the tick period
// (first retry is next tick, preserving the historical behavior for a
// transient decline); the cap keeps a permanently-declining gate visible in
// the skip telemetry without hammering its host command.
const (
	gateSkipBackoffBase = 30 * time.Second
	gateSkipBackoffCap  = 30 * time.Minute
)

// gateSkipBackoff returns the delay before a declined gate is re-evaluated,
// given how many skips the task has already recorded.
func gateSkipBackoff(skipCount int) time.Duration {
	d := gateSkipBackoffBase
	for i := 0; i < skipCount && d < gateSkipBackoffCap; i++ {
		d *= 2
	}
	if d > gateSkipBackoffCap {
		d = gateSkipBackoffCap
	}
	return d
}

// maxSkipReasonLogBytes bounds the gate stderr echoed into the task_skipped
// LOG line; the full (8 KiB-capped) reason persists to last_skip_reason, so
// diagnosis is unaffected.
const maxSkipReasonLogBytes = 256

func clampSkipReason(reason string) string {
	if len(reason) <= maxSkipReasonLogBytes {
		return reason
	}
	// json.Marshal replaces a rune split by the byte clamp with U+FFFD, so the
	// cut is safe even mid-UTF-8.
	return reason[:maxSkipReasonLogBytes] + "…[truncated]"
}
