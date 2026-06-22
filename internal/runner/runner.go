// Package runner is the in-process capped worker pool. It folds gig's remote
// register/heartbeat/HTTP-lease protocol into a single in-box pool:
//
//   - a global semaphore (FLEET_MAX_CONCURRENT_AGENTS, default 4) bounds
//     simultaneous agents across the whole process;
//   - ClaimNextPendingTask uses FOR UPDATE SKIP LOCKED to lease the next
//     pending task to one synthetic in-box lease owner (a sentinel UUID),
//     replacing gig's node UUIDs and the HTTP /tasks/pending poll;
//   - a per-process lease-renew ticker renews active leases well inside the
//     5-minute window (heartbeats are gone);
//   - RecoverExpiredLeases is the crash-safe backstop: a systemd restart
//     mid-task lets the lease expire and the task re-queues for re-claim;
//   - graceful drain on shutdown waits on a taskWG so in-flight tasks finish
//     reporting their terminal status + logs (via a background context).
//
// gig's `podman run cutlass` container launch is REPLACED by an in-process
// call to the scheduled driver (TaskRunner); tools still run in the sandbox.
// Status and logs become direct internal/sched/storage writes — no HTTP hop.
package runner

import (
	"context"
	"log"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

const (
	// DefaultMaxConcurrentAgents bounds simultaneous agents when
	// FLEET_MAX_CONCURRENT_AGENTS is unset/invalid (plan §6.4).
	DefaultMaxConcurrentAgents = 4

	// defaultPollInterval is how often an idle pool checks for pending work.
	defaultPollInterval = 30 * time.Second

	// defaultLeaseRenewInterval renews active leases well inside the 5-minute
	// lease window (storage.LeaseDuration) since heartbeats are gone.
	defaultLeaseRenewInterval = 90 * time.Second
)

// TaskRunner executes one claimed task in-process. The production impl
// constructs an agent.Agent (Mode=Scheduled) from config + the task's
// mcp_selection + the sandbox pool and calls Execute; tests inject a fake. It
// returns the run result/error; the pool owns status + log persistence.
type TaskRunner interface {
	// Run executes the task to completion. The returned LogSession (may be nil)
	// is persisted by the pool; a non-nil error marks the task errored.
	Run(ctx context.Context, task *models.Task) (*models.LogSession, error)
}

// TaskRunnerFunc adapts a function to TaskRunner.
type TaskRunnerFunc func(ctx context.Context, task *models.Task) (*models.LogSession, error)

// Run implements TaskRunner.
func (f TaskRunnerFunc) Run(ctx context.Context, task *models.Task) (*models.LogSession, error) {
	return f(ctx, task)
}

// Config configures the pool.
type Config struct {
	// MaxConcurrentAgents is the global cap. 0 → read FLEET_MAX_CONCURRENT_AGENTS
	// (default DefaultMaxConcurrentAgents).
	MaxConcurrentAgents int
	// PollInterval is how often to poll for pending tasks. 0 → default.
	PollInterval time.Duration
	// LeaseRenewInterval is how often active leases are renewed. 0 → default.
	LeaseRenewInterval time.Duration
}

// Pool is the in-process capped worker pool.
type Pool struct {
	store  *storage.Storage
	runner TaskRunner

	// sem is THE global cap: a buffered channel of cap slots. Acquire BEFORE
	// claiming/running a task, release AFTER cleanup.
	sem chan struct{}

	pollInterval       time.Duration
	leaseRenewInterval time.Duration

	// leaseOwner identifies this process's synthetic in-box worker. A fixed
	// per-process UUID so UpdateTaskStatusAtomic's lease-ownership check
	// (lease_owner == owner) and RecoverExpiredLeases both work unchanged.
	leaseOwner uuid.UUID

	// taskWG tracks in-flight task goroutines so Shutdown drains them.
	taskWG sync.WaitGroup

	// active tracks tasks currently executing (by lease token) for lease renewal.
	mu     sync.Mutex
	active map[uuid.UUID]uuid.UUID // task ID → per-claim lease token
}

// NewPool builds a pool over a storage layer and a task runner.
func NewPool(store *storage.Storage, runner TaskRunner, cfg Config) *Pool {
	capacity := cfg.MaxConcurrentAgents
	if capacity <= 0 {
		capacity = maxConcurrentFromEnv()
	}
	poll := cfg.PollInterval
	if poll <= 0 {
		poll = defaultPollInterval
	}
	renew := cfg.LeaseRenewInterval
	if renew <= 0 {
		renew = defaultLeaseRenewInterval
	}
	return &Pool{
		store:              store,
		runner:             runner,
		sem:                make(chan struct{}, capacity),
		pollInterval:       poll,
		leaseRenewInterval: renew,
		leaseOwner:         uuid.New(),
		active:             make(map[uuid.UUID]uuid.UUID),
	}
}

// maxConcurrentFromEnv reads FLEET_MAX_CONCURRENT_AGENTS, validating it like
// cutlass's iteration bound (a positive integer), falling back to the default.
func maxConcurrentFromEnv() int {
	v := os.Getenv("FLEET_MAX_CONCURRENT_AGENTS")
	if v == "" {
		return DefaultMaxConcurrentAgents
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		log.Printf("⚠ Ignoring invalid FLEET_MAX_CONCURRENT_AGENTS=%q; using default %d", v, DefaultMaxConcurrentAgents)
		return DefaultMaxConcurrentAgents
	}
	return n
}

// Cap returns the configured global concurrency cap.
func (p *Pool) Cap() int { return cap(p.sem) }

// LeaseOwner returns this process's synthetic worker identity.
func (p *Pool) LeaseOwner() uuid.UUID { return p.leaseOwner }

// Run drives the pool until ctx is cancelled, then drains in-flight tasks. It
// runs the claim loop and the lease-renew ticker; it blocks until shutdown
// completes (taskWG drained), so callers run it in its own goroutine or as the
// process's main loop.
func (p *Pool) Run(ctx context.Context) {
	renewTicker := time.NewTicker(p.leaseRenewInterval)
	defer renewTicker.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-renewTicker.C:
				p.renewActiveLeases()
			}
		}
	}()

	ticker := time.NewTicker(p.pollInterval)
	defer ticker.Stop()

	// Poll immediately on startup rather than waiting a full interval.
	for {
		p.tryClaim(ctx)
		select {
		case <-ctx.Done():
			log.Println("runner: draining in-flight tasks...")
			p.taskWG.Wait()
			log.Println("runner: shutdown complete")
			return
		case <-ticker.C:
		}
	}
}

// tryClaim acquires a cap slot (non-blocking) and, if one is free, claims and
// runs one pending task. The semaphore is THE cap: when full, this poll is a
// no-op and the extra work stays pending. The drain-loop keeps claiming while
// slots free up, so a single tick can launch up to cap tasks.
func (p *Pool) tryClaim(ctx context.Context) {
	for {
		select {
		case p.sem <- struct{}{}: // acquire BEFORE claiming (blocks at cap → we use select to stay non-blocking)
		default:
			return // at cap: leave the rest pending
		}

		task, err := p.store.ClaimNextPendingTask(ctx, p.leaseOwner.String())
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("runner: claim error: %v", err)
			}
			<-p.sem
			return
		}
		if task == nil {
			// Nothing to claim: release the slot and stop this tick.
			<-p.sem
			return
		}

		// Per-claim lease token: a goroutine whose lease was recovered must not
		// clobber a fresh claim's state. We tag the active map and re-verify
		// ownership before terminal writes.
		token := uuid.New()
		p.mu.Lock()
		p.active[task.ID] = token
		p.mu.Unlock()

		p.taskWG.Add(1)
		go func(task *models.Task, token uuid.UUID) {
			defer p.taskWG.Done()
			defer func() {
				p.mu.Lock()
				delete(p.active, task.ID)
				p.mu.Unlock()
				<-p.sem // release AFTER cleanup
			}()
			p.executeTask(ctx, task, token)
		}(task, token)
		// Loop to claim another task if a slot is still free (drains a burst).
	}
}

// executeTask runs one claimed task in-process via the TaskRunner, then writes
// its terminal status + log directly to storage. Status/log writes use a
// background context so they still land during shutdown after ctx is cancelled.
func (p *Pool) executeTask(ctx context.Context, task *models.Task, token uuid.UUID) {
	start := time.Now()

	// Report running (sets StartedAt + renews lease).
	if _, err := p.reportStatus(task.ID, models.TaskStatusRunning, "Starting task execution"); err != nil {
		log.Printf("runner: failed to report running for task %s: %v", task.ID, err)
	}

	session, runErr := p.runner.Run(ctx, task)

	// If our lease was recovered out from under us (another claim now owns the
	// task), do not clobber its state.
	if !p.stillOwns(task.ID, token) {
		log.Printf("runner: task %s lease no longer held (recovered); skipping terminal write", task.ID)
		return
	}

	// Interrupted only when the run itself failed AND shutdown was in progress:
	// a runner that returned cleanly finished its work even if ctx was cancelled
	// mid-drain, so we record its real outcome (the drain waits for it).
	interrupted := runErr != nil && ctx.Err() != nil
	switch {
	case interrupted:
		msg := "Task interrupted: runner shut down before completion"
		if _, err := p.reportStatus(task.ID, models.TaskStatusError, msg); err != nil {
			log.Printf("runner: failed to report interrupt for task %s: %v", task.ID, err)
		}
		p.submitLog(task, session, msg)
		log.Printf("runner: task %s interrupted after %v", task.ID, time.Since(start).Round(time.Second))
	case runErr != nil:
		msg := "Task failed: " + runErr.Error()
		if _, err := p.reportStatus(task.ID, models.TaskStatusError, msg); err != nil {
			log.Printf("runner: failed to report error for task %s: %v", task.ID, err)
		}
		p.submitLog(task, session, msg)
		log.Printf("runner: task %s failed after %v: %v", task.ID, time.Since(start).Round(time.Second), runErr)
	default:
		if _, err := p.reportStatus(task.ID, models.TaskStatusSuccess, "Task completed successfully"); err != nil {
			log.Printf("runner: failed to report success for task %s: %v", task.ID, err)
		}
		p.submitLog(task, session, "")
		log.Printf("runner: task %s completed in %v", task.ID, time.Since(start).Round(time.Second))
	}
}

// reportStatus writes a status update for the synthetic worker using a
// background context (shutdown-safe).
func (p *Pool) reportStatus(taskID uuid.UUID, status models.TaskStatus, message string) (*models.Task, error) {
	var msgPtr *string
	if message != "" {
		msgPtr = &message
	}
	return p.store.UpdateTaskStatusAtomicWithContext(context.Background(), taskID, p.leaseOwner, &models.StatusUpdate{
		TaskID:  taskID,
		Status:  status,
		Message: msgPtr,
	})
}

// submitLog persists the run's session log. When the runner produced no
// session (early failure), a synthetic one-message log is stored so the failure
// is visible, mirroring gig's submitSyntheticErrorLog.
func (p *Pool) submitLog(task *models.Task, session *models.LogSession, failureReason string) {
	if session == nil {
		now := time.Now().Unix()
		session = &models.LogSession{
			ID:        "session-synthetic-" + task.ID.String(),
			Title:     "Task Failure",
			CreatedAt: now,
			UpdatedAt: now,
			Messages: []models.LogMessage{
				{ID: task.ID.String() + "-0", Role: "user", Content: task.Prompt, CreatedAt: now},
			},
		}
		if failureReason != "" {
			et := "error"
			session.Messages = append(session.Messages, models.LogMessage{
				ID: task.ID.String() + "-1", Role: "user", Content: "[fatal] " + failureReason, CreatedAt: now, MessageType: &et,
			})
		}
	}
	if _, err := p.store.AddLogWithContext(context.Background(), task.ID, session); err != nil {
		log.Printf("runner: failed to submit logs for task %s: %v", task.ID, err)
	}
}

// stillOwns reports whether the pool still holds task with the given claim token
// (the active-map entry hasn't been replaced by a re-claim after recovery).
func (p *Pool) stillOwns(taskID, token uuid.UUID) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur, ok := p.active[taskID]
	return ok && cur == token
}

// renewActiveLeases re-asserts running for every in-flight task so the
// orchestrator doesn't expire their leases mid-run. Replaces gig's
// heartbeat-driven renewActiveTaskLease.
func (p *Pool) renewActiveLeases() {
	p.mu.Lock()
	ids := make([]uuid.UUID, 0, len(p.active))
	for id := range p.active {
		ids = append(ids, id)
	}
	p.mu.Unlock()

	for _, id := range ids {
		if _, err := p.reportStatus(id, models.TaskStatusRunning, ""); err != nil {
			log.Printf("runner: lease renewal failed for task %s: %v", id, err)
		}
	}
}

// RecoverExpiredLeases re-queues tasks whose lease expired (crash recovery). The
// scheduler ticker also calls this; the pool exposes it for tests and for
// startup recovery.
func (p *Pool) RecoverExpiredLeases() (int, error) {
	return p.store.RecoverExpiredLeases()
}
