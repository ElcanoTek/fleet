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
	"encoding/json"
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
	"github.com/ElcanoTek/fleet/internal/tools"
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
	// ErrorAnalyzer, when set, runs a post-failure LLM diagnosis (#317) for tasks
	// that fail TERMINALLY, off-thread. nil (the default) disables analysis — the
	// fire path is then a cheap no-op, byte-for-byte unchanged. The analyzer is
	// fired from a detached, time-bounded goroutine; its errors NEVER affect task
	// status or the pool's bookkeeping (mirrors Notifier).
	ErrorAnalyzer ErrorAnalyzer
	// EmailReplier, when set, sends a reply to an inbound-email trigger's sender
	// when an email-spawned run succeeds (#511 reply-back). nil (the default)
	// disables reply-back — the fire path is a cheap no-op. Fired from a detached,
	// time-bounded goroutine; its errors NEVER affect task status (mirrors Notifier).
	EmailReplier EmailReplier
}

// ErrorAnalyzer produces a structured post-failure diagnosis for a terminally
// failed task (#317). The runner passes primitives only (no models.LogSession,
// no agent types) so the seam stays decoupled from the agent package — the
// implementation lives in internal/agent and is injected in main.go. It returns
// validated JSON ({category, summary, remediation}) the runner persists verbatim,
// or an error (logged, no persistence). Implementations MUST honor ctx (the
// runner bounds it) and must not panic.
type ErrorAnalyzer interface {
	AnalyzeTaskFailure(ctx context.Context, taskPrompt, errMsg, sessionTail string) (json.RawMessage, error)
}

// EmailReplier sends a reply to an inbound-email trigger's original sender when
// an email-spawned run succeeds (#511 reply-back). Primitive params keep the seam
// decoupled from the notify package; the implementation is *notify.Notifier,
// injected in main.go. Implementations MUST honor ctx and must not panic; a nil
// return means sent (or a no-op when SMTP isn't configured), a non-nil error is
// logged and never affects task status.
type EmailReplier interface {
	ReplyToEmailEvent(ctx context.Context, to, subject, body, inReplyTo string) error
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

	// active tracks tasks currently executing (lease token + the per-task
	// cancel an operator Stop fires, #508) for lease renewal / stillOwns /
	// StopTask. mu also guards taskCancel and stopRequested.
	mu     sync.Mutex
	active map[uuid.UUID]activeRun
	// pauseRequested records the QUESTION a running task's agent posed via `ask`
	// (#510), set from the run context's ask handler; executeTask parks the
	// task in paused_awaiting_input (releasing the sandbox/lease) instead of a
	// terminal write. mu guards it alongside active/stopRequested.
	pauseRequested map[uuid.UUID]string
	// stopRequested records WHO asked a task to stop (task ID → operator
	// label) between StopTask firing the cancel and executeTask classifying
	// the outcome — the marker that routes the run to the "stopped" terminal
	// branch instead of retry/dead-letter (and instead of the shutdown
	// "interrupted" label).
	stopRequested map[uuid.UUID]string

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

	// errorAnalyzer runs the post-failure LLM diagnosis (#317). nil = analysis off
	// (the fire path is a no-op). Fired off-thread, time-bounded; never affects
	// task status.
	errorAnalyzer ErrorAnalyzer

	// emailReplier sends a reply to an inbound-email trigger's sender when an
	// email-spawned run succeeds (#511 reply-back). nil = reply-back off (the fire
	// path is a no-op). Fired off-thread, time-bounded; never affects task status.
	emailReplier EmailReplier
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
		active:             make(map[uuid.UUID]activeRun),
		stopRequested:      make(map[uuid.UUID]string),
		pauseRequested:     make(map[uuid.UUID]string),
		streams:            newTaskStreamRegistry(),
		notifier:           cfg.Notifier,
		publicURLBase:      strings.TrimRight(cfg.PublicURLBase, "/"),
		errorAnalyzer:      cfg.ErrorAnalyzer,
		emailReplier:       cfg.EmailReplier,
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

// activeRun is one in-flight task's bookkeeping: the per-claim lease token
// (terminal-write fencing) and the per-task cancel (#508 operator stop).
type activeRun struct {
	token  uuid.UUID
	cancel context.CancelFunc
}

// StopTask requests an operator stop of one running task (#508): it records
// who asked (the attribution executeTask writes into the terminal record) and
// cancels that task's context. The run halts at the governed loop's next
// checkpoint — an in-flight sandbox exec is killed via its context, the
// sandbox/MCP client are returned by the existing defers, and the partial
// session log still persists (submitLog is lease-free). Returns false when
// the task is not executing in this process.
func (p *Pool) StopTask(taskID uuid.UUID, who string) bool {
	p.mu.Lock()
	entry, ok := p.active[taskID]
	if ok {
		if strings.TrimSpace(who) == "" {
			who = "operator"
		}
		p.stopRequested[taskID] = who
	}
	p.mu.Unlock()
	if !ok {
		return false
	}
	entry.cancel()
	return true
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
		// Per-task cancellable context derived from the pool-wide taskCtx: a
		// shutdown still cancels every task, and StopTask (#508) can now cancel
		// exactly one without touching its neighbors.
		runTaskCtx, cancelTask := context.WithCancel(taskCtx)
		p.mu.Lock()
		p.active[task.ID] = activeRun{token: token, cancel: cancelTask}
		p.mu.Unlock()

		p.taskWG.Add(1)
		go func(task *models.Task, token uuid.UUID, release func()) {
			defer p.taskWG.Done()
			defer func() {
				cancelTask() // release the per-task context on every exit path
				p.mu.Lock()
				delete(p.active, task.ID)
				delete(p.stopRequested, task.ID)
				delete(p.pauseRequested, task.ID)
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
			// Run on the decoupled per-task context (not the claim ctx) so a
			// shutdown lets this task finish naturally up to the grace period
			// and an operator StopTask halts only this task.
			p.executeTask(runTaskCtx, task, token)
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
	// Resumed-after-ask (#510): the run injects the pending Q&A from the
	// in-memory task struct; clear the DB columns now (under our lease) so a
	// later run doesn't re-inject a stale answer. Best-effort.
	if task.PendingQuestion != "" || task.PendingAnswer != "" {
		if err := p.store.ClearPendingQA(context.Background(), task.ID, p.leaseOwner); err != nil {
			log.Printf("runner: failed to clear pending Q&A for task %s: %v", task.ID, err)
		}
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
	// Collect the named artifacts the agent publishes via publish_artifact (#204);
	// persisted on the success path below, before the terminal transition clears
	// the lease.
	artifactColl := scheduledrun.NewArtifactCollector()
	runCtx = scheduledrun.WithArtifactCollector(runCtx, artifactColl)
	// ask/notify (#510): the ask handler records the question + cancels THIS
	// task's run so it ends and releases the sandbox/lease; executeTask then
	// parks the task in paused_awaiting_input. notify fires an out-of-band
	// progress update and returns immediately (the run continues).
	runCtx = tools.WithAskHandler(runCtx, func(question string) error {
		p.mu.Lock()
		p.pauseRequested[task.ID] = question
		cancel := p.activeCancel(task.ID)
		p.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return nil
	})
	runCtx = tools.WithNotifyHandler(runCtx, func(message string) {
		p.notifyProgress(task, message)
	})
	// Recurring context carry (#504): for a carry_context recurring task, install
	// a bounded handoff from the prior run so scheduledrun injects a
	// "## Previous Run" section (extracted to keep executeTask under gocyclo).
	runCtx = p.withPriorRunContext(runCtx, task)

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

	// Operator stop (#508): consume the attribution marker StopTask recorded
	// BEFORE classifying the outcome. This must come first — a cancelled
	// agentcore run returns a nil error with a partial session (Result.Cancelled
	// is dropped by the scheduled driver), so without the marker an operator
	// stop would be mislabeled as success.
	p.mu.Lock()
	stoppedBy, wasStopped := p.stopRequested[task.ID]
	delete(p.stopRequested, task.ID)
	pauseQuestion, wasPaused := p.pauseRequested[task.ID]
	delete(p.pauseRequested, task.ID)
	p.mu.Unlock()

	// Emit a terminal lifecycle status (the always-last frame). The deferred release
	// seals the buffer so attached clients see EOF; the registry retains it briefly.
	termStatus := "succeeded"
	switch {
	case wasPaused:
		termStatus = "paused"
	case wasStopped:
		termStatus = "stopped"
	case runErr != nil || taskCtx.Err() != nil:
		termStatus = "failed"
	}
	var costUSD float64
	if session != nil {
		costUSD = session.Cost
	}
	terminalFrame := map[string]any{
		"type": "status", "status": termStatus, "task_id": task.ID.String(), "cost_usd": costUSD,
	}
	if wasStopped {
		terminalFrame["stopped_by"] = stoppedBy
	}
	buf.Emit("status", terminalFrame)

	if wasPaused {
		// ask (#510): park the task awaiting a human answer. The lease-guarded
		// pause clears the lease so no sandbox is held while waiting; the
		// partial transcript persists (submitLog is lease-free) and an
		// out-of-band notification tells the human a task needs them. NOT a
		// failure — no retry/dead-letter/error-analysis.
		if ok, err := p.store.PauseTaskForQuestion(context.Background(), task.ID, p.leaseOwner, pauseQuestion); err != nil {
			log.Printf("runner: task %s pause write failed: %v", task.ID, err)
		} else if !ok {
			log.Printf("runner: task %s pause did not apply (lease lost or not running)", task.ID)
		}
		p.submitLog(task, session, "Paused awaiting human input: "+pauseQuestion)
		p.notifyProgress(task, "Task is paused and needs your answer: "+pauseQuestion)
		log.Printf("runner: task %s paused awaiting input after %v", task.ID, time.Since(start).Round(time.Second))
		return
	}

	if wasStopped {
		// The cancel handler already flipped the row to cancelled (with the
		// "stopped by <who>" attribution) and cleared the lease, so there is no
		// terminal status write here — just persist the partial transcript
		// (submitLog is lease-free) and skip retry/dead-letter/notify/analysis:
		// a deliberate operator stop is not a failure to diagnose.
		msg := "Task stopped by " + stoppedBy
		p.submitLog(task, session, msg)
		log.Printf("runner: task %s stopped by %s after %v", task.ID, logSafeRunner(stoppedBy), time.Since(start).Round(time.Second))
		return
	}

	// If our lease was recovered out from under us (another claim now owns the
	// task), do not clobber its state.
	if !p.stillOwns(task.ID, token) {
		log.Printf("runner: task %s lease no longer held (recovered); skipping terminal write", task.ID)
		return
	}

	// Interrupted when the task context was cancelled — with the decoupled
	// per-task ctx that happens ONLY when the shutdown grace period expired (or
	// ForceCancel fired); the operator-stop case returned above. runErr is NOT
	// required: a cancelled agentcore run reports Cancelled via a nil error, so
	// requiring runErr here used to mislabel a force-cancelled single-pass run
	// as success with a truncated transcript. (The narrow race — a run that
	// completed fully in the same instant the grace expired — now records as
	// interrupted; re-running a completed task is safer than trusting a
	// possibly-truncated "success".)
	interrupted := taskCtx.Err() != nil
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
		// Terminal failure (quarantined): fire the outbound notification (#208) and
		// the post-failure LLM diagnosis (#317), both off-thread.
		p.notifyTerminal(task, notify.StatusFailure, session, time.Since(start))
		p.maybeAnalyzeFailure(task, session, runErr)
	case runErr != nil:
		// Non-retryable (deterministic) failure: there is no point retrying, so route
		// straight to the dead-letter queue (#253). This replaces the prior bare-error
		// terminal write — a deterministic config/validation failure now quarantines
		// for review rather than silently erroring.
		reason := "non-retryable failure: " + runErr.Error()
		p.sendToDeadLetter(task, session, runErr, reason, "non_retryable", start)
		// Terminal failure (quarantined): fire the outbound notification (#208) and
		// the post-failure LLM diagnosis (#317), both off-thread.
		p.notifyTerminal(task, notify.StatusFailure, session, time.Since(start))
		p.maybeAnalyzeFailure(task, session, runErr)
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
		// Persist the published-artifact manifest (#204) under the held lease,
		// before the terminal success clears it. No-op when nothing was published.
		p.recordArtifacts(task.ID, artifactColl.Marshal())
		if _, err := p.reportStatus(task.ID, models.TaskStatusSuccess, "Task completed successfully"); err != nil {
			log.Printf("runner: failed to report success for task %s: %v", task.ID, err)
		}
		p.submitLog(task, session, "")
		log.Printf("runner: task %s completed in %v", task.ID, time.Since(start).Round(time.Second))
		// Terminal success: fire the outbound notification off-thread (#208).
		p.notifyTerminal(task, notify.StatusSuccess, session, time.Since(start))
		// If this run answered an inbound email (#511), reply to the sender with
		// the result. Off-thread, no-op unless the run came from an email trigger.
		p.maybeReplyToEmailEvent(task, session)
	}
}

// logSafeRunner strips CR/LF from operator-supplied text before it lands in a
// log line (the handlers' logSafe pattern).
func logSafeRunner(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
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

// recordArtifacts persists the published-artifact manifest (#204) the run's
// agent produced via publish_artifact, riding a TaskStatusRunning update under
// the held lease exactly like recordStructuredOutput — BEFORE the terminal
// success clears the lease. An empty manifest persists nothing (the column
// stays NULL and GET /tasks/{id}/artifacts 404s); a persist failure is logged
// and swallowed so the run still succeeds.
func (p *Pool) recordArtifacts(taskID uuid.UUID, manifest json.RawMessage) {
	if len(manifest) == 0 {
		return
	}
	if _, err := p.store.UpdateTaskStatusAtomicWithContext(context.Background(), taskID, p.leaseOwner, &models.StatusUpdate{
		TaskID:    taskID,
		Status:    models.TaskStatusRunning,
		Artifacts: manifest,
	}); err != nil {
		log.Printf("runner: task %s failed to persist artifacts: %v", taskID, err)
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

// activeCancel returns the per-task cancel func for an in-flight task (#510
// ask uses it to end its own run). Caller holds p.mu.
func (p *Pool) activeCancel(taskID uuid.UUID) context.CancelFunc {
	if r, ok := p.active[taskID]; ok {
		return r.cancel
	}
	return nil
}

// notifyProgress fires a non-blocking, out-of-band progress notification for a
// task (#510 notify / ask-pause). Off-thread + no-op when no notifier is wired.
func (p *Pool) notifyProgress(task *models.Task, message string) {
	if p.notifier == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		ev := notify.Event{
			Status:  notify.StatusProgress,
			TaskID:  task.ID.String(),
			Name:    notifyTaskName(task.Prompt),
			Message: message,
			// Owner email = the push audience (#292), resolved off-thread like
			// notifyTerminal so the ask/notify path never waits on the lookup.
			Audience: p.ownerEmail(ctx, task),
		}
		if p.publicURLBase != "" {
			ev.LogURL = p.publicURLBase + "/orchestrator/tasks/" + task.ID.String()
		}
		if err := p.notifier.Notify(ctx, ev); err != nil {
			log.Printf("runner: progress notify for task %s failed: %v", task.ID, err)
		}
	}()
}

// maybeReplyToEmailEvent replies to an inbound-email trigger's sender with the
// run's result when an email-spawned run succeeds (#511 reply-back). Everything
// (the event lookup and the send) runs off-thread and time-bounded so the
// terminal path is never blocked; it is a no-op when reply-back is unwired, when
// the run did not originate from an email trigger event (the lookup returns
// sql.ErrNoRows), or when the event has no recorded sender. Its error is logged
// and NEVER affects task status (mirrors notifyTerminal / maybeAnalyzeFailure).
func (p *Pool) maybeReplyToEmailEvent(task *models.Task, session *models.LogSession) {
	if p.emailReplier == nil {
		return
	}
	body := strings.TrimSpace(finalAssistantText(session))
	if body == "" {
		return // nothing to send back
	}
	replier := p.emailReplier
	taskID := task.ID
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		ev, err := p.store.GetTriggerEventByRunID(ctx, taskID)
		if err != nil || ev == nil || strings.TrimSpace(ev.Sender) == "" {
			return // not an email-triggered run, or no reply target
		}
		if err := replier.ReplyToEmailEvent(ctx, ev.Sender, ev.Subject, body, ev.MessageID); err != nil {
			log.Printf("runner: email reply for task %s failed: %v", taskID, err)
		}
	}()
}

// withPriorRunContext installs the recurring context-carry handoff (#504) on the
// run context when the task opted in AND is recurring: the prior run's bounded
// final answer, injected by scheduledrun as a "## Previous Run" section. A
// no-op (returns ctx unchanged) for one-shot / non-carry tasks or a first run
// with no prior log. Extracted from executeTask to keep it under gocyclo.
func (p *Pool) withPriorRunContext(ctx context.Context, task *models.Task) context.Context {
	if !task.CarryContext || strings.TrimSpace(task.Recurrence) == "" {
		return ctx
	}
	prior := p.priorRunHandoff(task.ID)
	if prior == "" {
		return ctx
	}
	return scheduledrun.WithPriorRunContext(ctx, prior)
}

// priorRunHandoff returns a bounded handoff from a task's PRIOR run — its final
// assistant message clamped to carryContextMaxChars — for recurring
// context-carry (#504). Empty when there is no prior run or no answer.
// Deterministic + cheap: it reads the already-persisted last session, no LLM.
func (p *Pool) priorRunHandoff(taskID uuid.UUID) string {
	session, err := p.store.GetLog(taskID)
	if err != nil || session == nil {
		return ""
	}
	var last string
	for i := len(session.Messages) - 1; i >= 0; i-- {
		if session.Messages[i].Role == "assistant" && strings.TrimSpace(session.Messages[i].Content) != "" {
			last = strings.TrimSpace(session.Messages[i].Content)
			break
		}
	}
	if len(last) > carryContextMaxChars {
		last = last[:carryContextMaxChars] + "…[truncated]"
	}
	return last
}

// carryContextMaxChars bounds the prior-run handoff so context-carry stays
// cheap and deterministic (no whole-transcript replay).
const carryContextMaxChars = 2000

// stillOwns reports whether the pool still holds task with the given claim token
// (the active-map entry hasn't been replaced by a re-claim after recovery).
func (p *Pool) stillOwns(taskID, token uuid.UUID) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur, ok := p.active[taskID]
	return ok && cur.token == token
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
