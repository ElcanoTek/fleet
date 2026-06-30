// Package runner is the in-process capped worker pool. It folds gig's remote
// register/heartbeat/HTTP-lease protocol into a single in-box pool:
//
//   - a global semaphore (FLEET_MAX_CONCURRENT_AGENTS, default 8) bounds
//     simultaneous SCHEDULED tasks across the whole process (interactive chat
//     turns are not gated by it — they take a sandbox on demand);
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
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/admission"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/metrics"
	"github.com/ElcanoTek/fleet/internal/notify"
	"github.com/ElcanoTek/fleet/internal/observability"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/scheduledrun"
	"github.com/ElcanoTek/fleet/internal/structuredoutput"
)

const (
	// DefaultMaxConcurrentAgents bounds simultaneous scheduled tasks when
	// FLEET_MAX_CONCURRENT_AGENTS is unset/invalid. fleet is built to scale
	// vertically on one large box, so the default is generous; raise the env var
	// to match a bigger host (see the README sizing table).
	DefaultMaxConcurrentAgents = 8

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
	// Limiter, when set, is the SHARED process-wide admission governor (interactive
	// chat + scheduled tasks). The pool admits scheduled tasks through it, so total
	// in-flight turns stay within the box-wide cap and scheduled work never consumes
	// the slots reserved for interactive chat. When nil, the pool builds a private
	// limiter from MaxConcurrentAgents (reserving nothing) — the standalone behavior
	// tests rely on.
	Limiter *admission.Limiter
	// MaxConcurrentAgents is the global cap used only when Limiter is nil. 0 → read
	// FLEET_MAX_CONCURRENT_AGENTS (default DefaultMaxConcurrentAgents).
	MaxConcurrentAgents int
	// PollInterval is how often to poll for pending tasks. 0 → default.
	PollInterval time.Duration
	// LeaseRenewInterval is how often active leases are renewed. 0 → default.
	LeaseRenewInterval time.Duration
	// DrainGrace bounds how long Run waits, after its ctx is cancelled, for
	// in-flight tasks to finish NATURALLY before force-cancelling them. 0 →
	// defaultDrainGrace. A negative value means "force-cancel immediately" (no
	// wait) — the fast SIGINT/dev-exit path; ForceCancel does the same on demand.
	DrainGrace time.Duration
	// Notifier, when set, receives an outbound completion notification each time a
	// task reaches a terminal status (#208). nil (the default) disables
	// notifications entirely — the fire path becomes a cheap no-op. The notifier is
	// fired from a detached goroutine; its errors NEVER affect task status.
	Notifier Notifier
	// PublicURLBase is the absolute base URL (scheme+host, no trailing slash) used
	// to build the per-task log link in notifications, e.g.
	// https://fleet.example.com. Empty omits the link. Only consulted when Notifier
	// is set.
	PublicURLBase string
}

// Pool is the in-process capped worker pool.
type Pool struct {
	store  *storage.Storage
	runner TaskRunner

	// streams holds the live per-task SSE event buffers (#200). executeTask
	// registers a buffer before a run and seals it after, tee'ing the run's event
	// stream into it via agentcore.WithStreamObserver; the orchestrator's
	// GET /tasks/{id}/stream handler attaches clients through StreamRegistry.
	streams *TaskStreamRegistry

	// limiter is the shared admission governor. tryClaim admits scheduled tasks
	// through TryAcquireScheduled (non-blocking); when the scheduler is at its
	// sub-cap — or the whole box is full — the claim is a no-op and work stays
	// pending until a slot frees.
	limiter *admission.Limiter

	pollInterval       time.Duration
	leaseRenewInterval time.Duration

	// drainGrace bounds the post-shutdown wait for in-flight tasks to finish
	// naturally before they are force-cancelled (see Run / drainWithGrace).
	drainGrace time.Duration

	// leaseOwner identifies this process's synthetic in-box worker. A fixed
	// per-process UUID so UpdateTaskStatusAtomic's lease-ownership check
	// (lease_owner == owner) and RecoverExpiredLeases both work unchanged.
	leaseOwner uuid.UUID

	// taskWG tracks in-flight task goroutines so Shutdown drains them.
	taskWG sync.WaitGroup

	// active tracks tasks currently executing (by lease token) for lease renewal.
	// mu also guards taskCancel.
	mu     sync.Mutex
	active map[uuid.UUID]uuid.UUID // task ID → per-claim lease token

	// taskCancel cancels the context shared by all in-flight task executions. It
	// is decoupled from Run's ctx so a shutdown signal stops NEW claims at once
	// while letting running tasks finish up to drainGrace; it fires only when the
	// grace period expires or ForceCancel is called. nil until Run installs it.
	taskCancel context.CancelFunc

	// notifier delivers outbound completion notifications on terminal status
	// (#208). nil = notifications OFF (the default); the fire path is then a cheap
	// no-op. publicURLBase builds the per-task log link when set.
	notifier      Notifier
	publicURLBase string
}

// defaultDrainGrace bounds the shutdown wait for in-flight tasks when Config
// leaves DrainGrace unset.
const defaultDrainGrace = 30 * time.Second

// NewPool builds a pool over a storage layer and a task runner.
func NewPool(store *storage.Storage, runner TaskRunner, cfg Config) *Pool {
	limiter := cfg.Limiter
	if limiter == nil {
		capacity := cfg.MaxConcurrentAgents
		if capacity <= 0 {
			capacity = maxConcurrentFromEnv()
		}
		// Standalone pool (no shared limiter): reserve nothing, so the scheduler
		// may use the whole cap — the legacy behavior the runner's own tests assert.
		limiter = admission.New(capacity, 0)
	}
	poll := cfg.PollInterval
	if poll <= 0 {
		poll = defaultPollInterval
	}
	renew := cfg.LeaseRenewInterval
	if renew <= 0 {
		renew = defaultLeaseRenewInterval
	}
	// DrainGrace: 0 → default; a negative value is preserved (force-cancel
	// immediately, no natural-completion wait) for the fast-exit path.
	grace := cfg.DrainGrace
	if grace == 0 {
		grace = defaultDrainGrace
	}
	return &Pool{
		store:              store,
		runner:             runner,
		limiter:            limiter,
		pollInterval:       poll,
		leaseRenewInterval: renew,
		drainGrace:         grace,
		leaseOwner:         uuid.New(),
		active:             make(map[uuid.UUID]uuid.UUID),
		streams:            newTaskStreamRegistry(),
		notifier:           cfg.Notifier,
		publicURLBase:      strings.TrimRight(cfg.PublicURLBase, "/"),
	}
}

// StreamRegistry returns the pool's live per-task SSE stream registry (#200). The
// orchestrator wires it into the handlers' GET /tasks/{id}/stream lookup so a
// client can tail an in-progress task's run log.
func (p *Pool) StreamRegistry() *TaskStreamRegistry { return p.streams }

// maxConcurrentFromEnv reads FLEET_MAX_CONCURRENT_AGENTS, validating it like
// cutlass's iteration bound (a positive integer), falling back to the default.
func maxConcurrentFromEnv() int {
	v := os.Getenv("FLEET_MAX_CONCURRENT_AGENTS")
	if v == "" {
		return DefaultMaxConcurrentAgents
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		//nolint:gosec // G706 false positive: v is rendered with %q, which escapes any CR/LF, so it cannot forge log lines. v is also an operator-set env var, not request input.
		log.Printf("⚠ Ignoring invalid FLEET_MAX_CONCURRENT_AGENTS=%q; using default %d", v, DefaultMaxConcurrentAgents)
		return DefaultMaxConcurrentAgents
	}
	return n
}

// Cap returns the max number of scheduled tasks that may run concurrently
// (the shared limiter's schedulable slots = total - interactive reserve).
func (p *Pool) Cap() int { return p.limiter.SchedulableSlots() }

// LeaseOwner returns this process's synthetic worker identity.
func (p *Pool) LeaseOwner() uuid.UUID { return p.leaseOwner }

// Run drives the pool until ctx is cancelled, then drains in-flight tasks. It
// runs the claim loop and the lease-renew ticker; it blocks until shutdown
// completes (taskWG drained), so callers run it in its own goroutine or as the
// process's main loop.
func (p *Pool) Run(ctx context.Context) {
	// taskCtx is the parent context for in-flight task execution, decoupled from
	// ctx: cancelling ctx (a shutdown signal) stops NEW claims immediately, but
	// running tasks keep their context until the grace period expires — so a task
	// finishing within drainGrace records its real outcome instead of being marked
	// interrupted. taskCancel fires on grace expiry (below) or via ForceCancel.
	taskCtx, taskCancel := context.WithCancel(context.Background())
	defer taskCancel()
	p.mu.Lock()
	p.taskCancel = taskCancel
	p.mu.Unlock()

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
		p.tryClaim(ctx, taskCtx)
		select {
		case <-ctx.Done():
			log.Printf("runner: draining in-flight tasks (grace %s)...", p.drainGrace)
			if p.drainWithGrace(p.drainGrace) {
				log.Println("runner: all in-flight tasks drained")
			} else {
				log.Printf("runner: grace period (%s) expired; force-cancelling in-flight tasks", p.drainGrace)
				taskCancel()
				p.taskWG.Wait()
			}
			log.Println("runner: shutdown complete")
			return
		case <-ticker.C:
		}
	}
}

// drainWithGrace waits up to grace for the in-flight task WaitGroup to drain.
// It returns true if the tasks drained in time, false if grace expired first
// (the caller then force-cancels). A non-positive grace means "do not wait" —
// force-cancel immediately (fast exit), returning false.
func (p *Pool) drainWithGrace(grace time.Duration) bool {
	if grace <= 0 {
		return false
	}
	done := make(chan struct{})
	go func() {
		p.taskWG.Wait()
		close(done)
	}()
	t := time.NewTimer(grace)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

// ForceCancel cancels the in-flight task context immediately, regardless of the
// grace period — the fast-exit path (SIGINT / dev Ctrl-C / listener error).
// In-flight tasks see ctx.Err() at their next checkpoint and exit. Safe to call
// before Run installs the cancel (no-op) and idempotent afterwards.
func (p *Pool) ForceCancel() {
	p.mu.Lock()
	c := p.taskCancel
	p.mu.Unlock()
	if c != nil {
		c()
	}
}

// ActiveTasks reports the number of tasks currently executing — the diagnostic
// counter behind the SIGUSR1 status log.
func (p *Pool) ActiveTasks() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.active)
}

// tryClaim acquires a scheduler slot from the shared limiter (non-blocking) and,
// if one is free, claims and runs one pending task. The limiter is THE cap: when
// the scheduler sub-cap is reached (or the box is full of interactive turns),
// this poll is a no-op and the extra work stays pending. The drain-loop keeps
// claiming while slots free up, so a single tick can launch up to the sub-cap.
func (p *Pool) tryClaim(ctx, taskCtx context.Context) {
	for {
		release, ok := p.limiter.TryAcquireScheduled() // acquire BEFORE claiming (non-blocking)
		if !ok {
			return // at the scheduler sub-cap or the box is full: leave the rest pending
		}

		task, err := p.store.ClaimNextPendingTask(ctx, p.leaseOwner.String())
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("runner: claim error: %v", err)
			}
			release()
			return
		}
		if task == nil {
			// Nothing to claim: release the slot and stop this tick.
			release()
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
		go func(task *models.Task, token uuid.UUID, release func()) {
			defer p.taskWG.Done()
			defer func() {
				p.mu.Lock()
				delete(p.active, task.ID)
				p.mu.Unlock()
				release() // release AFTER cleanup
			}()
			// Recover so a panic in task execution fails only this task, not the
			// whole single-host process. Registered last → runs first on unwind:
			// mark the task errored (if still owned) so it isn't stuck running
			// until lease expiry, then the cleanup defers free the slot. The
			// Sentry capture ships a structured event with task_id / model /
			// attempt tags so the issue is filterable in the Sentry UI (#193).
			// observability.CapturePanic is a cheap no-op when FLEET_SENTRY_DSN
			// is unset (the SDK checks internally), so the default config pays
			// nothing for the call.
			defer safe.Recover("runner.worker", func(val any) {
				if p.stillOwns(task.ID, token) {
					if _, err := p.reportStatus(task.ID, models.TaskStatusError, "task panicked during execution"); err != nil {
						log.Printf("runner: failed to mark panicked task %s errored: %v", task.ID, err)
					}
				}
				model := ""
				if task.Model != nil {
					model = *task.Model
				}
				observability.CapturePanic(ctx, val, func(s *sentry.Scope) {
					s.SetTag("task_id", task.ID.String())
					s.SetTag("model", model)
					s.SetTag("flavor", "native-inprocess")
					s.SetContext("task", sentry.Context{
						"attempt": task.AttemptCount,
					})
				})
			})
			// Run on the decoupled taskCtx (not the claim ctx) so a shutdown lets
			// this task finish naturally up to the grace period.
			p.executeTask(taskCtx, task, token)
		}(task, token, release)
		// Loop to claim another task if a slot is still free (drains a burst).
	}
}

// executeTask runs one claimed task in-process via the TaskRunner, then writes
// its terminal status + log directly to storage. taskCtx is the decoupled
// task-execution context (cancelled only on grace expiry / ForceCancel), NOT the
// claim ctx; status/log writes use a background context so they still land during
// shutdown after taskCtx is cancelled.
func (p *Pool) executeTask(taskCtx context.Context, task *models.Task, token uuid.UUID) {
	start := time.Now()

	// Sentry breadcrumb (#193): the task-start trail so a captured panic's
	// event in the Sentry UI shows what the runner did immediately before the
	// crash. No-op when FLEET_SENTRY_DSN is unset (the SDK checks internally).
	model := ""
	if task.Model != nil {
		model = *task.Model
	}
	observability.AddBreadcrumb(taskCtx, "runner", "task start: "+task.ID.String(), map[string]string{
		"model":   model,
		"attempt": strconv.Itoa(task.AttemptCount),
	})

	// Register a live SSE buffer so GET /tasks/{id}/stream can attach + tail this
	// run (#200). The buffer is tee'd into the run's Observer event stream via
	// taskCtx below, sealed after the run, and retained briefly for late joiners.
	// It is purely in-memory and additive — the authoritative log is still written
	// to storage by submitLog at completion exactly as before.
	buf := p.streams.register(task.ID)
	// Seal + retain the buffer no matter how executeTask returns (including a panic
	// in the run, which safe.Recover in tryClaim catches AFTER this defer seals the
	// buffer) so attached clients always see EOF rather than hanging. release is
	// idempotent, so the explicit terminal-status seal below is the normal path and
	// this defer is the safety net.
	defer p.streams.release(task.ID, buf)
	buf.Emit("status", map[string]any{
		"type": "status", "status": "running", "task_id": task.ID.String(),
	})

	// Report running (sets StartedAt + renews lease).
	if _, err := p.reportStatus(task.ID, models.TaskStatusRunning, "Starting task execution"); err != nil {
		log.Printf("runner: failed to report running for task %s: %v", task.ID, err)
	}

	// Install the workspace-path reporter (#287): the scheduled runner invokes it
	// once it has resolved this run's effective workspace directory (a per-run
	// worktree subdir, or the shared workspace root), and we persist that path to
	// the task row under our held lease so the file-browser endpoints can later
	// list + stream the artifacts the agent produced. Reporting failure is
	// non-fatal — it only disables the after-the-fact browser for this run.
	runCtx := agentcore.WithStreamObserver(taskCtx, buf)
	runCtx = scheduledrun.WithWorkspaceReporter(runCtx, func(_ context.Context, path string) {
		p.reportWorkspacePath(task.ID, path)
	})

	session, runErr := p.runner.Run(runCtx, task)

	if runErr != nil && !errors.Is(runErr, agentcore.ErrRetryBudgetExhausted) && !errors.Is(runErr, agentcore.ErrStreamBlipPersisted) {
		observability.CaptureException(taskCtx, runErr, func(s *sentry.Scope) {
			s.SetTag("task_id", task.ID.String())
			s.SetTag("model", model)
			s.SetTag("flavor", "native-inprocess")
			s.SetContext("task", sentry.Context{
				"attempt": task.AttemptCount,
			})
		})
	}

	// Emit a terminal lifecycle status (the always-last frame). The deferred release
	// seals the buffer so attached clients see EOF; the registry retains it briefly.
	termStatus := "succeeded"
	if runErr != nil {
		termStatus = "failed"
	}
	var costUSD float64
	if session != nil {
		costUSD = session.Cost
	}
	buf.Emit("status", map[string]any{
		"type": "status", "status": termStatus, "task_id": task.ID.String(), "cost_usd": costUSD,
	})

	// If our lease was recovered out from under us (another claim now owns the
	// task), do not clobber its state.
	if !p.stillOwns(task.ID, token) {
		log.Printf("runner: task %s lease no longer held (recovered); skipping terminal write", task.ID)
		return
	}

	// Interrupted only when the run itself failed AND the task context was
	// cancelled — which, with the decoupled taskCtx, happens ONLY when the
	// shutdown grace period expired (or ForceCancel fired). A task that returns
	// during the grace window keeps its full context and records its real outcome;
	// a long task that outlasts the grace is force-cancelled here and re-queues via
	// lease expiry on the next start.
	interrupted := runErr != nil && taskCtx.Err() != nil
	switch {
	case interrupted:
		msg := "Task interrupted: server shutdown (grace period expired)"
		if _, err := p.reportStatus(task.ID, models.TaskStatusError, msg); err != nil {
			log.Printf("runner: failed to report interrupt for task %s: %v", task.ID, err)
		}
		p.submitLog(task, session, msg)
		log.Printf("runner: task %s interrupted after %v", task.ID, time.Since(start).Round(time.Second))
		// Terminal failure: fire the outbound notification off-thread (#208).
		p.notifyTerminal(task, notify.StatusFailure, session, time.Since(start))
	case runErr != nil && task.RetryPolicy.ShouldRetryClass(classifyFailure(runErr)) && task.AttemptCount < task.MaxRetries:
		// Retryable failure class with retries left: re-queue the SAME task for
		// another whole-task attempt after a backoff, instead of failing terminally.
		// The next attempt re-binds MCP + runs the SAME governed loop via the normal
		// claim path. (AttemptCount/MaxRetries: MaxRetries is ADDITIONAL attempts, so
		// the task runs up to MaxRetries+1 times.) Which classes retry, and the
		// backoff curve, come from task.RetryPolicy (nil = legacy: transient only,
		// 30s→10m exponential) — see #201.
		class := classifyFailure(runErr)
		backoff := retryBackoff(task.AttemptCount, task.RetryPolicy)
		when := time.Now().UTC().Add(backoff)
		msg := fmt.Sprintf("Task attempt %d failed (%s); retrying in %s: %v",
			task.AttemptCount+1, class, backoff.Round(time.Second), runErr)
		if _, err := p.store.RequeueTaskForRetryWithContext(context.Background(), task.ID, p.leaseOwner, when, msg); err != nil {
			// Could not re-queue (e.g. lease lost): fall back to a terminal error so
			// the task never silently strands as running.
			log.Printf("runner: failed to re-queue task %s for retry: %v; marking error", task.ID, err)
			if _, rerr := p.reportStatus(task.ID, models.TaskStatusError, "Task failed: "+runErr.Error()); rerr != nil {
				log.Printf("runner: failed to report error for task %s: %v", task.ID, rerr)
			}
		} else {
			log.Printf("runner: task %s attempt %d failed (transient); re-queued for retry at %s",
				task.ID, task.AttemptCount+1, when.Format(time.RFC3339))
		}
		p.submitLog(task, session, msg)
	case runErr != nil && task.RetryPolicy.ShouldRetryClass(classifyFailure(runErr)):
		// Transient failure class, but retries are exhausted (the requeue case above
		// did not match because AttemptCount >= MaxRetries): route to the dead-letter
		// queue (#253) instead of bare error, so the exhausted task is reviewable and
		// replayable rather than indistinguishable from a one-off per-attempt error.
		reason := fmt.Sprintf("retry budget exhausted after %d attempt(s): %v", task.AttemptCount+1, runErr)
		p.sendToDeadLetter(task, session, runErr, reason, "retry_exhausted", start)
		// Terminal failure (quarantined): fire the outbound notification (#208).
		p.notifyTerminal(task, notify.StatusFailure, session, time.Since(start))
	case runErr != nil:
		// Non-retryable (deterministic) failure: there is no point retrying, so route
		// straight to the dead-letter queue (#253). This replaces the prior bare-error
		// terminal write — a deterministic config/validation failure now quarantines
		// for review rather than silently erroring.
		reason := "non-retryable failure: " + runErr.Error()
		p.sendToDeadLetter(task, session, runErr, reason, "non_retryable", start)
		// Terminal failure (quarantined): fire the outbound notification (#208).
		p.notifyTerminal(task, notify.StatusFailure, session, time.Since(start))
	default:
		// Structured-output mode (#244): if the task declared an output_schema,
		// validate the agent's final answer against it and persist the validated
		// JSON BEFORE the terminal success (which clears the lease). Best-effort —
		// a missing/non-conforming result leaves output_json NULL and the run still
		// succeeds (the free-form session log retains the text); the
		// GET /tasks/{id}/output endpoint then 404s.
		if len(task.OutputSchema) > 0 {
			p.recordStructuredOutput(task, session)
		}
		if _, err := p.reportStatus(task.ID, models.TaskStatusSuccess, "Task completed successfully"); err != nil {
			log.Printf("runner: failed to report success for task %s: %v", task.ID, err)
		}
		p.submitLog(task, session, "")
		log.Printf("runner: task %s completed in %v", task.ID, time.Since(start).Round(time.Second))
		// Terminal success: fire the outbound notification off-thread (#208).
		p.notifyTerminal(task, notify.StatusSuccess, session, time.Since(start))
	}
}

// classifyFailure maps a clean run failure to a RetryPolicy failure class (#201).
// Only failures backed by a distinct agentcore sentinel are distinguishable;
// everything else (deterministic config errors like "no model configured",
// validation failures, etc.) is FailureTerminal — which the default policy never
// retries, keeping the idempotency risk bounded. The richer classes the issue
// envisions (timeout / governance / validation) await dedicated agentcore
// sentinels; until then they fall through to terminal.
func classifyFailure(err error) string {
	switch {
	case errors.Is(err, agentcore.ErrRetryBudgetExhausted), errors.Is(err, agentcore.ErrStreamBlipPersisted):
		return models.FailureTransient
	case errors.Is(err, agentcore.ErrCostCeilingExceeded):
		return models.FailureCostCeiling
	case errors.Is(err, agentcore.ErrContextBudgetExhausted):
		return models.FailureContextBudget
	default:
		return models.FailureTerminal
	}
}

// retryBackoff returns the delay before re-running after a retryable failure.
// The curve comes from the task's RetryPolicy (nil → legacy: 30s base, 10m cap,
// exponential): exponential doubles per attempt up to the cap; fixed uses the
// base every attempt. ±10% jitter avoids thundering-herd re-promotion. The result
// is always > 0 so the re-queued ScheduledFor is strictly in the future (the
// scheduler promotes only scheduled_for <= now), preventing a tight crash-loop.
func retryBackoff(attempt int, policy *models.RetryPolicy) time.Duration {
	initialSec, maxSec, exponential := policy.EffectiveBackoff()
	base := time.Duration(initialSec) * time.Second
	maxBackoff := time.Duration(maxSec) * time.Second

	d := base
	if exponential {
		d = maxBackoff
		if attempt >= 0 && attempt < 8 {
			if scaled := base << attempt; scaled > 0 && scaled < maxBackoff {
				d = scaled
			}
		}
	} else if d > maxBackoff {
		d = maxBackoff
	}
	if d <= 0 {
		d = time.Second // defensive: keep the re-queued time strictly in the future
	}
	//nolint:gosec // G404: jitter only spreads retry backoff to avoid thundering-herd re-promotion; not security-sensitive.
	jitter := time.Duration(rand.Int64N(int64(d/5))) - d/10 // ±10%
	return d + jitter
}

// sendToDeadLetter routes a terminally-failed task to the dead-letter queue
// (#253): it transitions the task to TaskStatusDeadLettered (recording the
// failure reason + total attempt count), writes the run log, and increments the
// DLQ metric labeled by the bounded reason class. If the storage transition fails
// (e.g. the lease was recovered out from under us), it falls back to a plain
// terminal error so the task never strands as running — preserving the
// invariant that every finished run lands in SOME terminal state.
func (p *Pool) sendToDeadLetter(task *models.Task, session *models.LogSession, runErr error, reason, reasonClass string, start time.Time) {
	attempts := task.AttemptCount + 1
	if _, err := p.store.DeadLetterTaskWithContext(context.Background(), task.ID, p.leaseOwner, reason, attempts); err != nil {
		log.Printf("runner: failed to dead-letter task %s: %v; falling back to error status", task.ID, err)
		if _, rerr := p.reportStatus(task.ID, models.TaskStatusError, "Task failed: "+runErr.Error()); rerr != nil {
			log.Printf("runner: failed to report fallback error for task %s: %v", task.ID, rerr)
		}
		p.submitLog(task, session, reason)
		return
	}
	p.submitLog(task, session, reason)
	metrics.RecordDeadLetterQueued(reasonClass)
	log.Printf("runner: task %s dead-lettered (%s) after %v and %d attempt(s): %v",
		task.ID, reasonClass, time.Since(start).Round(time.Second), attempts, runErr)
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

// reportWorkspacePath persists the per-run workspace path (#287) on the task row
// under our held lease. It rides on a TaskStatusRunning update (the task IS
// running when the scheduled runner reports its workspace) so the atomic
// lease-checked path persists WorkspacePath without changing the lifecycle. A
// failure is logged and swallowed — the file browser is a convenience, never a
// reason to fail a run.
func (p *Pool) reportWorkspacePath(taskID uuid.UUID, path string) {
	if strings.TrimSpace(path) == "" {
		return
	}
	if _, err := p.store.UpdateTaskStatusAtomicWithContext(context.Background(), taskID, p.leaseOwner, &models.StatusUpdate{
		TaskID:        taskID,
		Status:        models.TaskStatusRunning,
		WorkspacePath: &path,
	}); err != nil {
		log.Printf("runner: failed to record workspace path for task %s: %v", taskID, err)
	}
}

// recordStructuredOutput validates the agent's final answer against the task's
// declared output_schema (#244) and persists the validated JSON to output_json
// under the held lease, riding a TaskStatusRunning update exactly like
// reportWorkspacePath. Best-effort: anything that goes wrong (no final text, a
// non-conforming answer, a persist failure) is logged and swallowed so the run
// still completes successfully — output_json simply stays NULL.
func (p *Pool) recordStructuredOutput(task *models.Task, session *models.LogSession) {
	finalText := finalAssistantText(session)
	if strings.TrimSpace(finalText) == "" {
		log.Printf("runner: task %s declared output_schema but produced no final text; output_json left null", task.ID)
		return
	}
	out, err := structuredoutput.ValidateOutput(finalText, task.OutputSchema)
	if err != nil {
		log.Printf("runner: task %s structured output did not validate: %v; output_json left null", task.ID, err)
		return
	}
	if _, err := p.store.UpdateTaskStatusAtomicWithContext(context.Background(), task.ID, p.leaseOwner, &models.StatusUpdate{
		TaskID:     task.ID,
		Status:     models.TaskStatusRunning,
		OutputJSON: out,
	}); err != nil {
		log.Printf("runner: task %s failed to persist output_json: %v", task.ID, err)
	}
}

// finalAssistantText returns the content of the last assistant message in the
// session — the agent's final answer — or "" when there is none.
func finalAssistantText(session *models.LogSession) string {
	if session == nil {
		return ""
	}
	for i := len(session.Messages) - 1; i >= 0; i-- {
		if session.Messages[i].Role == "assistant" {
			return session.Messages[i].Content
		}
	}
	return ""
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
