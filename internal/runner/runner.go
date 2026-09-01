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
	"math/rand/v2" // nosemgrep: go.lang.security.audit.crypto.math_random.math-random-used -- used once, for +/-10% jitter on a retry interval (see rand.Int64N below). Nothing here is a secret, a token, or an identity; crypto/rand would only make backoff slower.
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	"github.com/ElcanoTek/fleet/internal/truncate"
)

const (
	// DefaultMaxConcurrentAgents bounds simultaneous scheduled tasks when
	// FLEET_MAX_CONCURRENT_AGENTS is unset/invalid. fleet is built to scale
	// vertically on one large box, so the default is generous; raise the env var
	// to match a bigger host (see the README sizing table).
	DefaultMaxConcurrentAgents = 8

	// defaultPollInterval is how often an idle pool checks for pending work.
	defaultPollInterval = 30 * time.Second

	// wakeMaxCycles caps how many times ONE task may park itself for a wake
	// (self-wake, docs/SELF-WAKE.md) over its lifetime — the backstop against
	// an agent sleep-looping a task forever. Generous on purpose: a daily
	// standing watch re-armed per run never accumulates cycles (each
	// recurrence occurrence is a new task row); only a single long-lived
	// watch task approaches it.
	wakeMaxCycles = 100

	// defaultLeaseRenewInterval renews active leases well inside the 5-minute
	// lease window (storage.LeaseDuration) since heartbeats are gone.
	defaultLeaseRenewInterval = 90 * time.Second

	// DefaultTaskWallClockTimeout bounds one scheduled run's TOTAL wall-clock
	// time when FLEET_TASK_WALL_TIMEOUT is unset (#724). The iteration cap and
	// cost/token ceilings bound loop *progress*, but a single hung tool call
	// inside an iteration used to hold its pool slot until an operator
	// intervened; this is the hard ceiling that frees it. 4h matches the v1
	// runner's wall-clock kill.
	DefaultTaskWallClockTimeout = 4 * time.Hour
)

// errTaskWallTimeout is the cancellation cause installed by the per-task
// wall-clock deadline (#724), so executeTask can tell a wall-timeout expiry
// apart from a shutdown/ForceCancel cancellation of the same context.
var errTaskWallTimeout = errors.New("task wall-clock timeout exceeded")

// errTaskLeaseLost is the cancellation cause installed when this run's lease
// is discovered lost (#1116): a renewal came back ErrTaskLeaseNotHeld, or the
// task was re-claimed by this same pool while the stale run was still
// executing. It lets executeTask log/persist the honest "lease lost" outcome
// instead of misattributing the cancel to a server shutdown.
var errTaskLeaseLost = errors.New("task lease lost; run cancelled to stop duplicate side effects")

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
	// AdmitScheduled, when set, is consulted before each claim: returning false
	// leaves pending work pending for this tick. It is the seam the disk
	// backpressure guard uses to shed BACKGROUND load while interactive chat
	// keeps running (see internal/diskguard) — the pool stays up, the queue
	// simply stops draining until the condition clears.
	//
	// nil (the default) always admits, so nothing changes for embedders that do
	// not wire a gate. The reason string is for the log line only; it must be
	// operator-facing text with no request-derived content.
	//
	// MUST be cheap and non-blocking: it runs on the claim path. The guard
	// caches its measurement precisely so this stays a mutex and a comparison.
	AdmitScheduled func() (ok bool, reason string)
	// WallClockTimeout bounds one scheduled run's total wall-clock time (#724):
	// on expiry the run's context is cancelled and the task fails with a clear,
	// DETERMINISTIC timeout error (never retried, never dead-letter-replayed as
	// transient). 0 → read FLEET_TASK_WALL_TIMEOUT (a Go duration; default 4h,
	// "0" disables). Negative → disabled (no per-run deadline), for tests and
	// embedders that manage their own bounds.
	WallClockTimeout time.Duration
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

	// kick wakes the claim loop ahead of the next poll tick (see Kick). A
	// 1-buffered channel so any burst of concurrent kicks coalesces into at
	// most one extra scan.
	kick chan struct{}

	// drainGrace bounds the post-shutdown wait for in-flight tasks to finish
	// naturally before they are force-cancelled (see Run / drainWithGrace).
	drainGrace time.Duration

	// leaseOwner is a stable test/diagnostic identity used by LeaseOwner and the
	// legacy helper wrappers below. Production claims persist activeRun.token as
	// their owner, so two claims by this process can never pass each other's DB
	// lease fence after recovery (no fixed-owner ABA).
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
	// wakeRequested records the wake spec a running task's agent set via
	// `sleep` / `wake_on_event` (self-wake, docs/SELF-WAKE.md), set from the
	// run context's wake handler; executeTask parks the task in
	// paused_awaiting_wake (releasing the sandbox/lease) instead of a
	// terminal write. mu guards it alongside pauseRequested.
	wakeRequested map[uuid.UUID]tools.WakeSpec
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

	// beforeSuccessCommit is a package-private deterministic test seam at the
	// post-stillOwns/pre-transaction boundary. Production leaves it nil.
	beforeSuccessCommit func(*models.Task, uuid.UUID)

	// wallTimeout is the resolved per-run wall-clock ceiling (#724). 0 = disabled
	// (no per-run deadline). See Config.WallClockTimeout.
	wallTimeout time.Duration

	// admitScheduled gates the claim path (see Config.AdmitScheduled). nil =
	// always admit. shedLogged de-duplicates the "holding back" log line so a
	// box shedding for an hour writes one line, not one per poll tick.
	admitScheduled func() (bool, string)
	shedLogged     atomic.Bool
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
	// Wall-clock ceiling (#724): 0 → the FLEET_TASK_WALL_TIMEOUT env knob
	// (default 4h, "0" disables); negative → disabled.
	wall := cfg.WallClockTimeout
	if wall == 0 {
		wall = wallTimeoutFromEnv()
	}
	if wall < 0 {
		wall = 0
	}
	return &Pool{
		store:              store,
		runner:             runner,
		limiter:            limiter,
		pollInterval:       poll,
		leaseRenewInterval: renew,
		kick:               make(chan struct{}, 1),
		drainGrace:         grace,
		leaseOwner:         uuid.New(),
		active:             make(map[uuid.UUID]activeRun),
		stopRequested:      make(map[uuid.UUID]string),
		pauseRequested:     make(map[uuid.UUID]string),
		wakeRequested:      make(map[uuid.UUID]tools.WakeSpec),
		streams:            newTaskStreamRegistry(),
		notifier:           cfg.Notifier,
		publicURLBase:      strings.TrimRight(cfg.PublicURLBase, "/"),
		errorAnalyzer:      cfg.ErrorAnalyzer,
		emailReplier:       cfg.EmailReplier,
		wallTimeout:        wall,
		admitScheduled:     cfg.AdmitScheduled,
	}
}

// wallTimeoutFromEnv reads FLEET_TASK_WALL_TIMEOUT (#724) as a Go duration
// (e.g. "4h", "90m"). Unset → DefaultTaskWallClockTimeout; "0" → disabled;
// invalid or negative → warn and keep the default, so a typo can never mean
// "unbounded".
func wallTimeoutFromEnv() time.Duration {
	v := strings.TrimSpace(os.Getenv("FLEET_TASK_WALL_TIMEOUT"))
	if v == "" {
		return DefaultTaskWallClockTimeout
	}
	if v == "0" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		//nolint:gosec // G706 false positive: v is rendered with %q (escapes CR/LF) and is an operator-set env var, not request input.
		log.Printf("⚠ Ignoring invalid FLEET_TASK_WALL_TIMEOUT=%q; using default %s", v, DefaultTaskWallClockTimeout)
		return DefaultTaskWallClockTimeout
	}
	return d
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

	if p.wallTimeout > 0 {
		log.Printf("runner: per-task wall-clock timeout ON (%s; FLEET_TASK_WALL_TIMEOUT, 0 disables)", p.wallTimeout)
	} else {
		log.Printf("runner: per-task wall-clock timeout OFF (FLEET_TASK_WALL_TIMEOUT=0)")
	}

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
		case <-p.kick:
		}
	}
}

// Kick wakes the claim loop to scan for pending work now instead of at the
// next poll tick. Call it after a write that makes a task immediately
// claimable (a create in pending status, a resume, a wake), so a synchronous
// caller — the A2A blocking unary send above all — is not handed up to a full
// pollInterval of dispatch latency before its run even starts. Non-blocking
// and coalescing (any burst collapses into one extra scan), safe from any
// goroutine, and a no-op signal when the pool is saturated: the scan admits
// through the same limiter as every poll tick.
func (p *Pool) Kick() {
	select {
	case p.kick <- struct{}{}:
	default:
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
// cancel and cancelCause end the SAME per-claim context: cancel with the
// plain context.Canceled cause (stop/ask/wake and the exit-path release),
// cancelCause with an explicit cause — the lease-lost paths (#1116) pass
// errTaskLeaseLost so executeTask can attribute the cancellation honestly.
// Because the context is per-claim, holding a snapshot of these funcs stays
// safe after the map entry is overwritten by a re-claim: cancelling the OLD
// claim's context can never touch the new claim's run.
type activeRun struct {
	token       uuid.UUID
	cancel      context.CancelFunc
	cancelCause context.CancelCauseFunc
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

// admitClaims consults the optional backpressure gate. It logs the transition in
// each direction exactly once, so an operator sees WHEN the box started and
// stopped holding work back without a poll-rate flood in between.
func (p *Pool) admitClaims() bool {
	if p.admitScheduled == nil {
		return true
	}
	ok, reason := p.admitScheduled()
	if ok {
		if p.shedLogged.CompareAndSwap(true, false) {
			log.Print("runner: resuming scheduled task claims (backpressure cleared)")
		}
		return true
	}
	if p.shedLogged.CompareAndSwap(false, true) {
		// reason is operator-facing text from the gate itself (see
		// Config.AdmitScheduled), never request input — no log-forgery risk.
		log.Printf("runner: holding back scheduled task claims: %s", reason)
	}
	return false
}

// tryClaim acquires a scheduler slot from the shared limiter (non-blocking) and,
// if one is free, claims and runs one pending task. The limiter is THE cap: when
// the scheduler sub-cap is reached (or the box is full of interactive turns),
// this poll is a no-op and the extra work stays pending. The drain-loop keeps
// claiming while slots free up, so a single tick can launch up to the sub-cap.
func (p *Pool) tryClaim(ctx, taskCtx context.Context) {
	// Backpressure gate (disk headroom today) — checked BEFORE the limiter so a
	// shedding box does not even take a slot. Work already in flight is left
	// alone: shedding stops the queue from draining, it never kills a run.
	if !p.admitClaims() {
		return
	}
	for {
		release, ok := p.limiter.TryAcquireScheduled() // acquire BEFORE claiming (non-blocking)
		if !ok {
			return // at the scheduler sub-cap or the box is full: leave the rest pending
		}

		// Persist a fresh owner for every claim, including reclaims by this same
		// process. The token therefore fences both the in-memory active entry and
		// every lease-checked database write against ABA after recovery.
		token := uuid.New()
		task, err := p.store.ClaimNextPendingTask(ctx, token.String())
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
		// clobber a fresh claim's state. It is both the active-map generation and
		// the persisted lease owner.
		// Per-task cancellable context derived from the pool-wide taskCtx: a
		// shutdown still cancels every task, and StopTask (#508) can now cancel
		// exactly one without touching its neighbors. WithCancelCause so the
		// lease-lost paths (#1116) can attribute their cancellation; the plain
		// cancel wrapper keeps the historical context.Canceled cause for
		// stop/ask/wake and the exit-path release.
		runTaskCtx, cancelTaskCause := context.WithCancelCause(taskCtx)
		cancelTask := func() { cancelTaskCause(nil) }
		p.mu.Lock()
		prev, hadPrev := p.active[task.ID]
		p.active[task.ID] = activeRun{token: token, cancel: cancelTask, cancelCause: cancelTaskCause}
		p.mu.Unlock()
		if hadPrev {
			// This claim overwrote a live entry: the previous claim's lease is
			// definitively gone (this claim just persisted a new owner), so its
			// run — if still executing — is a zombie whose external side effects
			// must stop NOW (#1116). This is the majority lease-loss ordering on
			// a single box: recovery re-queues and the same pool re-claims within
			// a poll tick, before the zombie's next renewal would get the
			// not-held verdict. Cancelling here closes that window to zero; the
			// cause-cancel is an idempotent no-op when the previous run already
			// ended (ask/wake/stop cancelled it themselves and its exit-path
			// cleanup simply hasn't deleted the entry yet).
			log.Printf("runner: task %s re-claimed while a previous run's bookkeeping was still active; cancelling the stale run (lease lost)", task.ID)
			prev.cancelCause(errTaskLeaseLost)
		}

		p.taskWG.Add(1)
		go func(task *models.Task, token uuid.UUID, release func()) {
			defer p.taskWG.Done()
			defer func() {
				cancelTask() // release the per-task context on every exit path
				p.mu.Lock()
				// Token-guarded cleanup (#581): only remove the bookkeeping
				// entries when THIS claim still owns them. After a lease
				// recovery + in-process re-claim, active[task.ID] belongs to
				// the fresh claim (a new token) — an unguarded delete here
				// would defeat stillOwns' fencing for the live run, stop its
				// lease renewal, and orphan StopTask.
				if cur, ok := p.active[task.ID]; ok && cur.token == token {
					delete(p.active, task.ID)
					delete(p.stopRequested, task.ID)
					delete(p.pauseRequested, task.ID)
					delete(p.wakeRequested, task.ID)
				}
				p.mu.Unlock()
				release() // release AFTER cleanup
			}()
			// Recover so a panic in task execution fails only this task, not the
			// whole single-host process. Registered last → runs first on unwind:
			// mark the task errored (if still owned) so it isn't stuck running
			// until lease expiry, then the cleanup defers free the slot. The
			// Sentry capture ships only a value-free panic class with task_id /
			// model / attempt tags. A recovered value may contain connector
			// credentials, so it never crosses the telemetry seam (#193, #795).
			// observability.CapturePanicClass is a cheap no-op when FLEET_SENTRY_DSN
			// is unset (the SDK checks internally), so the default config pays
			// nothing for the call.
			defer safe.Recover("runner.worker", func(val any) {
				if p.stillOwns(task.ID, token) {
					if _, err := p.reportStatusForLease(task.ID, token, models.TaskStatusError, "task panicked during execution"); err != nil {
						log.Printf("runner: failed to mark panicked task %s errored: %v", task.ID, err)
					}
				}
				model := ""
				if task.Model != nil {
					model = *task.Model
				}
				observability.CapturePanicClass(ctx, safe.PanicClass(val), func(s *sentry.Scope) {
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

	// Per-task wall-clock ceiling (#724): bound this run's TOTAL elapsed time.
	// The iteration cap and cost/token ceilings bound loop progress, but a
	// single hung tool call inside an iteration observes neither — this
	// deadline cancels the run's context so the slot is always reclaimed. The
	// distinct cause lets the terminal switch below tell "wall timeout" apart
	// from a shutdown cancellation of the same context.
	taskCtx, cancelWall := p.withWallDeadline(taskCtx)
	defer cancelWall()

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
	if _, err := p.reportStatusForLease(task.ID, token, models.TaskStatusRunning, "Starting task execution"); err != nil {
		log.Printf("runner: failed to report running for task %s: %v", task.ID, err)
	}
	// Resumed-after-ask (#510): the run injects the pending Q&A from the
	// in-memory task struct. The DB columns are cleared at the TERMINAL
	// transition (clearPendingQA), not here — clearing at run start lost the
	// human's answer when the resumed run failed retryably, because the retry
	// is a fresh claim that re-reads pending_answer (#582).

	runCtx, artifactColl := p.buildTaskRunContext(taskCtx, task, token, buf)

	session, runErr := p.runner.Run(runCtx, task)

	// Operator stop (#508): consume the attribution marker StopTask recorded
	// BEFORE classifying the outcome. This must come first — the scheduled
	// driver surfaces a cancelled run as an ErrRunCancelled-wrapped error
	// (#1105; it used to return nil, which mislabeled the stop as success), and
	// only the marker can attribute that generic cancel to "stopped by <who>"
	// instead of routing it through the failure machinery.
	p.mu.Lock()
	stoppedBy, wasStopped := p.stopRequested[task.ID]
	delete(p.stopRequested, task.ID)
	pauseQuestion, wasPaused := p.pauseRequested[task.ID]
	delete(p.pauseRequested, task.ID)
	wakeSpec, wasWakeParked := p.wakeRequested[task.ID]
	delete(p.wakeRequested, task.ID)
	p.mu.Unlock()
	// A wake park shares the ask pause's outcome shape for classification:
	// not a terminal success (no structured commit), not a failure to report.
	parked := wasPaused || wasWakeParked

	var outputJSON json.RawMessage
	if runEligibleForStructuredCommit(runErr, parked, wasStopped, taskCtx.Err()) {
		outputJSON, runErr = validateStructuredRunOutput(task, session)
	}

	if reportableRunFailure(runErr, parked, wasStopped) {
		captureRunFailure(taskCtx, task, model, runErr)
	}

	// Emit a terminal lifecycle status (the always-last frame). The deferred release
	// seals the buffer so attached clients see EOF; the registry retains it briefly.
	// Fail closed until the success transaction actually returns a landed row.
	// This also keeps a panic in terminal bookkeeping from emitting false success
	// while deferred cleanup seals the stream.
	termStatus := terminalStreamStatus(wasWakeParked, wasPaused, wasStopped)
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
	// Emit only when this worker's terminal path has finished. In particular, a
	// structured run must not tell an attached client "succeeded" before the
	// lease-checked output_json+success transaction has committed. This defer was
	// registered after the stream-release defer, so it remains the always-last
	// frame and is delivered immediately before EOF.
	defer func() { buf.Emit("status", terminalFrame) }()

	if wasWakeParked {
		p.parkForWake(task, session, wakeSpec, token, start)
		return
	}

	if wasPaused {
		p.parkForQuestion(task, session, pauseQuestion, token, start)
		return
	}

	if wasStopped {
		p.finishStopped(task, session, stoppedBy, start)
		return
	}

	// If our lease was recovered out from under us (another claim now owns the
	// task), do not clobber its state.
	if !p.stillOwns(task.ID, token) {
		terminalFrame["status"] = "failed"
		log.Printf("runner: task %s lease no longer held (recovered); skipping terminal write", task.ID)
		return
	}

	// Interrupted when the task context was cancelled — with the decoupled
	// per-task ctx that happens ONLY when the shutdown grace period expired,
	// ForceCancel fired, or the lease-lost cancel (#1116) fired; the
	// operator-stop case returned above, and the lease-lost cause is peeled
	// off into its own branch below, so what reaches the generic interrupted
	// branch really is a shutdown/ForceCancel. runErr is NOT
	// required: the ctx check keeps the interruption attribution even if a
	// driver drops the cancel (the scheduled driver now surfaces it as
	// ErrRunCancelled, #1105, but this branch must not depend on that), and it
	// keeps the narrow race — a run that completed fully in the same instant
	// the grace expired — recording as interrupted; re-running a completed task
	// is safer than trusting a possibly-truncated "success".
	interrupted := taskCtx.Err() != nil
	// Lease lost (#1116): renewActiveLeases (or a re-claim overwrite in
	// tryClaim, though that ordering usually fails stillOwns above) cancelled
	// this run because the row no longer carries its token — recovery
	// re-queued the task and a fresh attempt may already be running. Persist
	// the honest reason into the transcript (submitLog is lease-free) and stop:
	// no terminal status write (the lease is definitively gone, the write
	// could only bounce off the fence), no retry/dead-letter/notify/analysis —
	// the task's outcome now belongs to the fresh attempt.
	leaseLost := errors.Is(context.Cause(taskCtx), errTaskLeaseLost)
	// Wall-clock ceiling expiry (#724) is a DETERMINISTIC terminal failure: it
	// must be classified BEFORE the interrupted/retry cases (its cancellation
	// also sets taskCtx.Err()) and it never enters the transient-retry or
	// retry-exhausted branches — a run that hit the ceiling once would hit it
	// again, so retrying would only burn another full timeout window.
	wallExpired := errors.Is(context.Cause(taskCtx), errTaskWallTimeout)
	// The agent ended its own run with confirm_audit(success=false) (#1151).
	// Classified with the wall timeout rather than with runErr because it is
	// DETERMINISTIC in the same way: the run reached a conclusion about its own
	// work, so a retry spends another window to reach the same one. It must also
	// beat the transient-retry branches, which would otherwise re-queue a run
	// that told us plainly it was done.
	auditAborted := errors.Is(runErr, agentcore.ErrAuditAborted)
	switch {
	case wallExpired:
		p.failWallTimeout(task, session, token, start)
	case leaseLost:
		// Beats auditAborted: whatever the zombie run concluded about its
		// own work, the task's outcome now belongs to the fresh attempt,
		// and any terminal write here could only bounce off the fence.
		p.finishLeaseLost(task, session, start)
	case auditAborted:
		p.failAuditAborted(task, session, runErr, token, start)
	case interrupted:
		p.failInterrupted(task, session, token, start)
	case runErr != nil:
		p.handleRunFailure(task, session, runErr, token, start)
	default:
		p.finishSuccess(task, session, outputJSON, artifactColl, token, start, terminalFrame)
	}
}

// buildTaskRunContext derives one claimed task's run context from the decoupled
// per-task context, installing every run-scoped seam the drivers and tools
// read, and returns it with the artifact collector the success path
// (finishSuccess) persists from.
func (p *Pool) buildTaskRunContext(taskCtx context.Context, task *models.Task, token uuid.UUID, buf *taskStreamBuffer) (context.Context, *scheduledrun.ArtifactCollector) {
	// Install the workspace-path reporter (#287): the scheduled runner invokes it
	// once it has resolved this run's effective workspace directory (a per-run
	// worktree subdir, or the shared workspace root), and we persist that path to
	// the task row under our held lease so the file-browser endpoints can later
	// list + stream the artifacts the agent produced. Reporting failure is
	// non-fatal — it only disables the after-the-fact browser for this run.
	runCtx := agentcore.WithStreamObserver(taskCtx, buf)
	runCtx = scheduledrun.WithWorkspaceReporter(runCtx, func(_ context.Context, path string) {
		p.reportWorkspacePathForLease(task.ID, token, path)
	})
	// Collect the named artifacts the agent publishes via publish_artifact (#204);
	// persisted on the success path (finishSuccess), before the terminal
	// transition clears the lease.
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
	// self-wake (docs/SELF-WAKE.md): the wake handler mirrors ask — record the
	// spec + cancel THIS run so it ends and releases the sandbox/lease;
	// executeTask then parks the task in paused_awaiting_wake.
	runCtx = tools.WithWakeHandler(runCtx, p.wakeHandlerFor(task))
	// Recurring context carry (#504): for a carry_context recurring task, install
	// a bounded handoff from the prior run so scheduledrun injects a
	// "## Previous Run" section (extracted to keep executeTask under gocyclo).
	runCtx = p.withPriorRunContext(runCtx, task)
	return runCtx, artifactColl
}

// captureRunFailure ships a reportable run failure to Sentry with the same
// task_id/model/flavor tags as the panic path in tryClaim. The caller gates it
// on reportableRunFailure.
func captureRunFailure(ctx context.Context, task *models.Task, model string, runErr error) {
	observability.CaptureException(ctx, runErr, func(s *sentry.Scope) {
		s.SetTag("task_id", task.ID.String())
		s.SetTag("model", model)
		s.SetTag("flavor", "native-inprocess")
		s.SetContext("task", sentry.Context{
			"attempt": task.AttemptCount,
		})
	})
}

// parkForQuestion parks a run whose agent called ask (#510): the task awaits a
// human answer. The partial transcript persists FIRST (submitLog is
// lease-free), THEN the lease-guarded pause clears the lease — so the moment
// the task is visibly paused_awaiting_input its logs are already readable (a
// UI opening a just-paused task never sees an empty transcript, and the
// stoptask test's paused→logs assertion is race-free). An out-of-band
// notification tells the human a task needs them. NOT a failure — no
// retry/dead-letter/error-analysis.
func (p *Pool) parkForQuestion(task *models.Task, session *models.LogSession, pauseQuestion string, token uuid.UUID, start time.Time) {
	p.submitLog(task, session, "Paused awaiting human input: "+pauseQuestion)
	if ok, err := p.store.PauseTaskForQuestion(context.Background(), task.ID, token, pauseQuestion); err != nil {
		log.Printf("runner: task %s pause write failed: %v", task.ID, err)
	} else if !ok {
		log.Printf("runner: task %s pause did not apply (lease lost or not running)", task.ID)
	}
	p.notifyProgress(task, "Task is paused and needs your answer: "+pauseQuestion)
	log.Printf("runner: task %s paused awaiting input after %v", task.ID, time.Since(start).Round(time.Second))
}

// finishStopped records a deliberate operator stop (#508). The cancel handler
// already flipped the row to cancelled (with the "stopped by <who>"
// attribution) and cleared the lease, so there is no terminal status write
// here — just persist the partial transcript (submitLog is lease-free) and
// skip retry/dead-letter/notify/analysis: a deliberate operator stop is not a
// failure to diagnose.
func (p *Pool) finishStopped(task *models.Task, session *models.LogSession, stoppedBy string, start time.Time) {
	msg := "Task stopped by " + stoppedBy
	p.submitLog(task, session, msg)
	log.Printf("runner: task %s stopped by %s after %v", task.ID, logSafeRunner(stoppedBy), time.Since(start).Round(time.Second))
}

// finishLeaseLost records the honest outcome of a run cancelled by lease loss
// (#1116): transcript only — see the leaseLost classification in executeTask
// for why there is no terminal status write and no retry/notify/analysis.
func (p *Pool) finishLeaseLost(task *models.Task, session *models.LogSession, start time.Time) {
	msg := "Task run cancelled: this run's lease was lost (recovery re-queued the task; a fresh attempt owns it now)"
	p.submitLog(task, session, msg)
	log.Printf("runner: task %s run cancelled after lease loss, %v after start", task.ID, time.Since(start).Round(time.Second))
}

// finishSuccess commits the successful terminal transition: it persists the
// published-artifact manifest under the held lease, atomically commits
// output_json + success, and fires the success side effects ONLY when the
// terminal write actually landed (#580). terminalFrame is executeTask's
// deferred terminal SSE frame — it is flipped to "succeeded" only after the
// success transaction returns a landed row (fail closed).
func (p *Pool) finishSuccess(task *models.Task, session *models.LogSession, outputJSON json.RawMessage, artifactColl *scheduledrun.ArtifactCollector, token uuid.UUID, start time.Time, terminalFrame map[string]any) {
	// Persist the published-artifact manifest (#204) under the held lease,
	// before the terminal success clears it. No-op when nothing was published.
	p.recordArtifactsForLease(task.ID, token, artifactColl.Marshal())
	if p.beforeSuccessCommit != nil {
		p.beforeSuccessCommit(task, token)
	}
	landedTask, err := p.reportSuccess(task.ID, token, outputJSON, session)
	if err != nil {
		terminalFrame["status"] = "failed"
		log.Printf("runner: failed to commit success for task %s: %v; suppressing success side effects", task.ID, err)
		// Never borrow a success row to justify this run's notification/session
		// side effects. A Commit error is genuinely outcome-unknown: only a
		// reread that still shows this exact claim's token on a nonterminal row
		// proves the success did not land and permits the normal persistence-
		// failure path. Every terminal, differently owned, or unreadable state
		// remains ambiguous and suppresses all side effects.
		if errors.Is(err, storage.ErrTaskLeaseNotHeld) {
			return
		}
		current, readErr := p.store.GetTask(task.ID)
		currentClaim := readErr == nil && current != nil && current.AttemptCount == task.AttemptCount &&
			current.LeaseOwner != nil && *current.LeaseOwner == token.String()
		if !currentClaim || current.Status.IsTerminal() {
			return
		}
		// Only a declared structured contract gives this write failure the
		// structured-output persistence class. Preserve the historical
		// free-form behavior: a failed terminal status write suppresses
		// success side effects but is not reclassified as an output failure.
		if len(task.OutputSchema) == 0 {
			p.submitLog(task, session, err.Error())
			return
		}
		persistErr := fmt.Errorf("%w: commit validated output and success under lease: %w", agentcore.ErrStructuredOutputPersistence, err)
		p.submitLog(task, session, persistErr.Error())
		p.handleRunFailure(task, session, persistErr, token, start)
		return
	}
	if landedTask != nil {
		terminalFrame["status"] = "succeeded"
	}
	p.submitLog(task, session, "")
	log.Printf("runner: task %s completed in %v", task.ID, time.Since(start).Round(time.Second))
	// Terminal success side effects fire ONLY when the terminal write landed
	// (#580): if the lease was lost (operator cancel racing the finish, or a
	// recovery re-queue), the DB does not record this run as a success — a
	// "success" notification or an external email reply would be spurious,
	// and the re-queued run's own reply would make it a duplicate.
	if landedTask != nil {
		task = landedTask
		// Terminal success: fire the outbound notification off-thread (#208).
		p.notifyTerminal(task, notify.StatusSuccess, session, time.Since(start))
		// If this run answered an inbound email (#511), reply to the sender with
		// the result. Off-thread, no-op unless the run came from an email trigger.
		p.maybeReplyToEmailEvent(task, session)
	}
}

// terminalStreamStatus picks the terminal lifecycle frame's status label from
// the run's park/stop markers (extracted to keep executeTask under gocyclo).
// Fail closed to "failed": the success path overwrites it only after the
// success transaction returns a landed row.
func terminalStreamStatus(wasWakeParked, wasPaused, wasStopped bool) string {
	switch {
	case wasWakeParked:
		return "sleeping"
	case wasPaused:
		return "paused"
	case wasStopped:
		return "stopped"
	}
	return "failed"
}

func runEligibleForStructuredCommit(runErr error, wasPaused, wasStopped bool, contextErr error) bool {
	return runErr == nil && !wasPaused && !wasStopped && contextErr == nil
}

// reportableRunFailure gates Sentry capture: transient infra weather is
// non-actionable, and a surfaced cancel (agentcore.ErrRunCancelled, #1105) is
// an interruption the stop/interrupted/wall-timeout branches attribute — not an
// application failure. Without the cancel exclusion every graceful-shutdown
// drain and wall-timeout expiry would page. A budget stop
// (ErrCostCeilingExceeded) stays reportable, matching the structured-output
// path that has always surfaced it as an error.
//
// A self-audit abort (agentcore.ErrAuditAborted, #1151) is excluded for a
// different reason than the others: nothing malfunctioned. The agent inspected
// its own work, decided not to publish, and said so — that is the machinery
// working, and paging on it would train everyone to ignore the page.
func reportableRunFailure(runErr error, wasPaused, wasStopped bool) bool {
	return runErr != nil && !wasPaused && !wasStopped &&
		!transientAgentFailure(runErr) && !errors.Is(runErr, agentcore.ErrRunCancelled) &&
		!errors.Is(runErr, agentcore.ErrAuditAborted)
}

// validateStructuredRunOutput is defense in depth around TaskRunner
// implementations: production already returns a validated terminal value from
// agentcore.Run, but a custom runner cannot bypass the persisted contract and
// manufacture success. Empty schemas preserve free-form behavior.
//
// The session's OutputJSON (#797) is the authoritative candidate: the exact
// bytes agentcore validated, redacted once at the driver boundary. The final
// assistant TEXT is a display artifact whose redaction can corrupt or fail an
// already-valid contract — it is only a fallback for legacy sessions written
// by drivers that predate the handoff.
func validateStructuredRunOutput(task *models.Task, session *models.LogSession) (json.RawMessage, error) {
	if task == nil || len(task.OutputSchema) == 0 {
		return nil, nil
	}
	candidate := ""
	if session != nil {
		candidate = session.OutputJSON
	}
	fromHandoff := strings.TrimSpace(candidate) != ""
	if !fromHandoff {
		candidate = finalAssistantText(session)
	}
	if strings.TrimSpace(candidate) == "" {
		return nil, fmt.Errorf("%w: task produced no terminal assistant output", agentcore.ErrStructuredOutputMissing)
	}
	out, err := structuredoutput.ValidateOutput(candidate, task.OutputSchema)
	if err != nil {
		if fromHandoff {
			// agentcore validated these bytes before the handoff; the only
			// process-side mutation since is secret redaction. Fail loudly and
			// honestly rather than committing corrupted-but-schema-shaped output.
			return nil, fmt.Errorf("%w: validated output no longer satisfies the schema after redaction (the declared output carried redacted secret material): %w",
				agentcore.ErrStructuredOutputInvalid, err)
		}
		return nil, fmt.Errorf("%w: %w", agentcore.ErrStructuredOutputInvalid, err)
	}
	return out, nil
}

// withWallDeadline derives the per-run wall-clock deadline context (#724) from
// the per-task context. Disabled (wallTimeout == 0) → the context is returned
// unchanged with a no-op cancel. The errTaskWallTimeout cause is what lets
// executeTask distinguish a wall-timeout expiry from a shutdown/ForceCancel
// cancellation of the same context.
func (p *Pool) withWallDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if p.wallTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeoutCause(ctx, p.wallTimeout, errTaskWallTimeout)
}

// failAuditAborted records the terminal outcome for a run whose agent ended it
// with confirm_audit(success=false) (#1151).
//
// The production case: task 3d767956 built a complete, schema-valid 978 KB
// payload — 846 rows added, none dropped, schema hash matched — then declined
// to dispatch it, printed COMPLETE_WITH_FLAGS naming four quality flags, and
// closed with ABORTED_WITH_FLAGS. The task row said status: success, result:
// "Task completed successfully". The agent was right and said so clearly; the
// system discarded that and reported green, which is how a client dashboard
// froze for days with every run "succeeding".
//
// The agent's own summary becomes the result message, because it is the single
// most useful sentence anyone will read about this run. Deliberately NOT
// retried and never dead-lettered: the run reached a conclusion, and a retry
// would only spend another window reaching the same one.
func (p *Pool) failAuditAborted(task *models.Task, session *models.LogSession, runErr error, leaseOwner uuid.UUID, start time.Time) {
	msg := "Task aborted by its own self-audit"
	if detail := strings.TrimSpace(auditAbortDetail(runErr)); detail != "" {
		msg += ": " + detail
	}
	p.clearPendingQA(task, leaseOwner)
	landed := true
	if _, err := p.reportStatusForLease(task.ID, leaseOwner, models.TaskStatusError, msg); err != nil {
		landed = false
		log.Printf("runner: failed to report audit abort for task %s: %v", task.ID, err)
	}
	p.submitLog(task, session, msg)
	log.Printf("runner: task %s aborted by self-audit after %v", task.ID, time.Since(start).Round(time.Second))
	if landed {
		p.notifyTerminal(task, notify.StatusFailure, session, time.Since(start))
	}
}

// maxTerminalMessageRunes bounds model-authored text on its way into a column
// every task list renders. Generous enough for a real summary paragraph, short
// enough that one run cannot make the Operations Center unreadable.
const maxTerminalMessageRunes = 600

// truncateRunes bounds by RUNES, not bytes, so a multi-byte character is never
// cut in half into invalid UTF-8.
func truncateRunes(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}

// auditAbortDetail peels the sentinel off the driver's wrapped error, leaving
// the agent's user_visible_summary. Bounded because it is model-authored text
// landing in a column every task list renders.
func auditAbortDetail(runErr error) string {
	if runErr == nil {
		return ""
	}
	detail := strings.TrimSpace(strings.TrimPrefix(runErr.Error(), agentcore.ErrAuditAborted.Error()))
	detail = strings.TrimSpace(strings.TrimPrefix(detail, ":"))
	return truncateRunes(detail, maxTerminalMessageRunes)
}

// failInterrupted records the terminal state for a run the shutdown grace period
// (or a ForceCancel) killed mid-flight. Extracted from executeTask's switch to
// keep that function under the gocyclo ceiling; the behavior is unchanged, and
// it now reads as a sibling of the other two terminal-write helpers.
func (p *Pool) failInterrupted(task *models.Task, session *models.LogSession, leaseOwner uuid.UUID, start time.Time) {
	msg := "Task interrupted: server shutdown (grace period expired)"
	p.clearPendingQA(task, leaseOwner)
	landed := true
	if _, err := p.reportStatusForLease(task.ID, leaseOwner, models.TaskStatusError, msg); err != nil {
		landed = false
		log.Printf("runner: failed to report interrupt for task %s: %v", task.ID, err)
	}
	p.submitLog(task, session, msg)
	log.Printf("runner: task %s interrupted after %v", task.ID, time.Since(start).Round(time.Second))
	// Terminal failure: fire the outbound notification off-thread (#208) — only
	// when the terminal write actually landed (#580), so a lease lost out from
	// under us never produces a notification the DB contradicts.
	if landed {
		p.notifyTerminal(task, notify.StatusFailure, session, time.Since(start))
	}
}

// failWallTimeout records the deterministic terminal failure for a run that
// exceeded the wall-clock ceiling (#724): a clear timeout error on the task
// row, the partial transcript persisted, and the failure notification fired —
// but NEVER the transient-retry/dead-letter machinery (a run that hit the
// ceiling once would hit it again; retrying would only burn another window).
func (p *Pool) failWallTimeout(task *models.Task, session *models.LogSession, leaseOwner uuid.UUID, start time.Time) {
	msg := fmt.Sprintf("Task failed: exceeded the wall-clock timeout of %s (FLEET_TASK_WALL_TIMEOUT); the run was cancelled", p.wallTimeout)
	p.clearPendingQA(task, leaseOwner)
	landed := true
	if _, err := p.reportStatusForLease(task.ID, leaseOwner, models.TaskStatusError, msg); err != nil {
		landed = false
		log.Printf("runner: failed to report wall-clock timeout for task %s: %v", task.ID, err)
	}
	p.submitLog(task, session, msg)
	log.Printf("runner: task %s exceeded the wall-clock timeout (%s); failed after %v",
		task.ID, p.wallTimeout, time.Since(start).Round(time.Second))
	// Terminal failure: fire the outbound notification off-thread (#208) — only
	// when the terminal write landed (#580), matching every other branch.
	if landed {
		p.notifyTerminal(task, notify.StatusFailure, session, time.Since(start))
	}
}

// logSafeRunner strips CR/LF from operator-supplied text before it lands in a
// log line (the handlers' logSafe pattern).
func logSafeRunner(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}

// transientAgentFailure reports whether err carries one of agentcore's
// transient-infra sentinels: the run failed on provider/transport weather, not
// on anything deterministic. These classify FailureTransient (the whole-task
// RetryPolicy owns the re-run — for ErrCommittedSideEffects that re-run
// repeats already-executed tool side effects, which is exactly the opt-in
// max_retries grants) and are excluded from Sentry capture as non-actionable.
func transientAgentFailure(err error) bool {
	return errors.Is(err, agentcore.ErrRetryBudgetExhausted) ||
		errors.Is(err, agentcore.ErrStreamBlipPersisted) ||
		errors.Is(err, agentcore.ErrCommittedSideEffects)
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
	case transientAgentFailure(err):
		return models.FailureTransient
	case errors.Is(err, agentcore.ErrCostCeilingExceeded):
		return models.FailureCostCeiling
	case errors.Is(err, agentcore.ErrContextBudgetExhausted):
		return models.FailureContextBudget
	case errors.Is(err, agentcore.ErrStructuredOutputFormat):
		return models.FailureOutputFormat
	case errors.Is(err, agentcore.ErrStructuredOutputPersistence):
		return models.FailureOutputPersist
	default:
		return models.FailureTerminal
	}
}

// handleRunFailure is the single retry/DLQ classifier for both agent failures
// and the post-run atomic output+success commit. Structured formatting and
// persistence have separate explicit classes and are non-retryable by default;
// an operator may opt either into retry_on with the same whole-task side-effect
// caveat as every other explicit retry class.
func (p *Pool) handleRunFailure(task *models.Task, session *models.LogSession, runErr error, leaseOwner uuid.UUID, start time.Time) {
	class := classifyFailure(runErr)
	if task.RetryPolicy.ShouldRetryClass(class) && task.AttemptCount < task.MaxRetries {
		backoff := retryBackoff(task.AttemptCount, task.RetryPolicy)
		when := time.Now().UTC().Add(backoff)
		msg := fmt.Sprintf("Task attempt %d failed (%s); retrying in %s: %v",
			task.AttemptCount+1, class, backoff.Round(time.Second), runErr)
		// Pending Q&A deliberately survives a successful requeue (#582).
		if _, err := p.store.RequeueTaskForRetryWithContext(context.Background(), task.ID, leaseOwner, when, msg); err != nil {
			log.Printf("runner: failed to re-queue task %s for retry: %v; marking error", task.ID, err)
			p.clearPendingQA(task, leaseOwner)
			if _, rerr := p.reportStatusForLease(task.ID, leaseOwner, models.TaskStatusError, "Task failed: "+runErr.Error()); rerr != nil {
				log.Printf("runner: failed to report error for task %s: %v", task.ID, rerr)
			} else {
				p.notifyTerminal(task, notify.StatusFailure, session, time.Since(start))
				p.maybeAnalyzeFailure(task, session, runErr)
			}
		} else {
			log.Printf("runner: task %s attempt %d failed (%s); re-queued for retry at %s",
				task.ID, task.AttemptCount+1, class, when.Format(time.RFC3339))
		}
		p.submitLog(task, session, msg)
		return
	}

	p.clearPendingQA(task, leaseOwner)
	if task.RetryPolicy.ShouldRetryClass(class) {
		reason := fmt.Sprintf("retry budget exhausted after %d attempt(s) (%s): %v", task.AttemptCount+1, class, runErr)
		if p.sendToDeadLetter(task, session, runErr, reason, "retry_exhausted", leaseOwner, start) {
			p.notifyTerminal(task, notify.StatusFailure, session, time.Since(start))
			p.maybeAnalyzeFailure(task, session, runErr)
		}
		return
	}

	reason := fmt.Sprintf("non-retryable failure (%s): %v", class, runErr)
	if p.sendToDeadLetter(task, session, runErr, reason, class, leaseOwner, start) {
		p.notifyTerminal(task, notify.StatusFailure, session, time.Since(start))
		p.maybeAnalyzeFailure(task, session, runErr)
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
// invariant that every finished run lands in SOME terminal state. It reports
// whether a terminal state actually landed (dead-lettered, or the fallback
// error) so the caller can gate the failure notification + diagnosis on it
// (#580): when even the fallback is rejected the DB no longer records this
// run's outcome and no external side effect may fire.
func (p *Pool) sendToDeadLetter(task *models.Task, session *models.LogSession, runErr error, reason, reasonClass string, leaseOwner uuid.UUID, start time.Time) bool {
	attempts := task.AttemptCount + 1
	if _, err := p.store.DeadLetterTaskWithContext(context.Background(), task.ID, leaseOwner, reason, attempts); err != nil {
		log.Printf("runner: failed to dead-letter task %s: %v; falling back to error status", task.ID, err)
		landed := true
		if _, rerr := p.reportStatusForLease(task.ID, leaseOwner, models.TaskStatusError, "Task failed: "+runErr.Error()); rerr != nil {
			landed = false
			log.Printf("runner: failed to report fallback error for task %s: %v", task.ID, rerr)
		}
		p.submitLog(task, session, reason)
		return landed
	}
	p.submitLog(task, session, reason)
	metrics.RecordDeadLetterQueued(reasonClass)
	log.Printf("runner: task %s dead-lettered (%s) after %v and %d attempt(s): %v",
		task.ID, reasonClass, time.Since(start).Round(time.Second), attempts, runErr)
	return true
}

// wakeHandlerFor builds the run-context wake handler for one task: record the
// spec + fire the per-task cancel. The cycle cap is enforced HERE (the tool
// surfaces the error to the model and the run continues) so a confused agent
// can't sleep-loop a task forever; task.WakeCycles is the lifetime park
// counter incremented under the lease-guarded pause write.
func (p *Pool) wakeHandlerFor(task *models.Task) tools.WakeHandler {
	return func(spec tools.WakeSpec) error {
		if task.WakeCycles >= wakeMaxCycles {
			return fmt.Errorf("this task has already parked for a wake %d times (cap %d)", task.WakeCycles, wakeMaxCycles)
		}
		p.mu.Lock()
		p.wakeRequested[task.ID] = spec
		cancel := p.activeCancel(task.ID)
		p.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return nil
	}
}

// parkForWake parks a run that called sleep / wake_on_event (self-wake,
// docs/SELF-WAKE.md) until its deadline or event. Same ordering discipline as
// the ask pause: transcript FIRST (submitLog is lease-free), THEN the
// lease-guarded park clears the lease — a UI opening a just-parked task never
// sees an empty transcript. NOT a failure — no retry/dead-letter/
// error-analysis — and no out-of-band notification: sleeping is normal
// operation, not a request for human attention (the agent can `notify`
// beforehand if a human should know).
func (p *Pool) parkForWake(task *models.Task, session *models.LogSession, spec tools.WakeSpec, token uuid.UUID, start time.Time) {
	what := "until " + spec.WakeAt.UTC().Format(time.RFC3339)
	if spec.EventKey != "" {
		what = "for event " + strconv.Quote(spec.EventKey) + " (timeout " + spec.WakeAt.UTC().Format(time.RFC3339) + ")"
	}
	p.submitLog(task, session, "Sleeping "+what)
	if ok, err := p.store.PauseTaskForWake(context.Background(), task.ID, token, spec.WakeAt, spec.EventKey, spec.Note); err != nil {
		log.Printf("runner: task %s wake park write failed: %v", task.ID, err)
	} else if !ok {
		log.Printf("runner: task %s wake park did not apply (lease lost or not running)", task.ID)
	}
	log.Printf("runner: task %s sleeping %s after %v", task.ID, logSafeRunner(what), time.Since(start).Round(time.Second))
}

// clearPendingQA nulls a resumed task's pending question/answer columns (#510)
// under our lease. It is called immediately BEFORE each terminal transition —
// never at run start — so a retryable failure re-queues the task with the
// human's answer intact and every retried attempt of the resumed run still
// injects it (#582). No-op when the task carried no pending Q&A; best-effort
// otherwise (a failure is logged and never affects the terminal write).
func (p *Pool) clearPendingQA(task *models.Task, leaseOwner uuid.UUID) {
	if task.PendingQuestion != "" || task.PendingAnswer != "" {
		if err := p.store.ClearPendingQA(context.Background(), task.ID, leaseOwner); err != nil {
			log.Printf("runner: failed to clear pending Q&A for task %s: %v", task.ID, err)
		}
	}
	// Wake state follows the same terminal-only clearing contract
	// (docs/SELF-WAKE.md): a retryable failure of a woken run re-queues with
	// the wake reason + note intact, so every retried attempt still injects
	// them. wake_cycles survives by design (lifetime cap counter).
	if task.WakeReason != "" || task.WakeNote != "" || task.WakeAt != nil {
		if err := p.store.ClearWakeState(context.Background(), task.ID, leaseOwner); err != nil {
			log.Printf("runner: failed to clear wake state for task %s: %v", task.ID, err)
		}
	}
}

// reportStatus writes a status update for the synthetic worker using a
// background context (shutdown-safe).
func (p *Pool) reportStatus(taskID uuid.UUID, status models.TaskStatus, message string) (*models.Task, error) {
	return p.reportStatusForLease(taskID, p.leaseOwner, status, message)
}

func (p *Pool) reportStatusForLease(taskID, leaseOwner uuid.UUID, status models.TaskStatus, message string) (*models.Task, error) {
	var msgPtr *string
	if message != "" {
		msgPtr = &message
	}
	return p.store.UpdateTaskStatusAtomicWithContext(context.Background(), taskID, leaseOwner, &models.StatusUpdate{
		TaskID:  taskID,
		Status:  status,
		Message: msgPtr,
	})
}

// reportSuccess atomically commits the terminal lifecycle transition together
// with validated output_json. Storage independently validates the effective
// value against output_schema, so success can never become visible first.
//
// The message is the agent's OWN closing summary when it wrote one (#1151). The
// constant "Task completed successfully" was written over a summary that said,
// in detail, that the page was unchanged and why — which was the single most
// useful field in the record, and the only one that would have told an operator
// a dashboard had stopped refreshing while every run showed green.
func (p *Pool) reportSuccess(taskID, leaseOwner uuid.UUID, output json.RawMessage, session *models.LogSession) (*models.Task, error) {
	msg := successMessage(session)
	return p.store.UpdateTaskStatusAtomicWithContext(context.Background(), taskID, leaseOwner, &models.StatusUpdate{
		TaskID:     taskID,
		Status:     models.TaskStatusSuccess,
		Message:    &msg,
		OutputJSON: output,
	})
}

// successMessage is the agent's final answer, bounded and flattened to one
// line, or the historical constant when the run produced no closing text (a
// structured-output run whose final text is its JSON keeps the constant too:
// the payload is already on the row in output_json, and repeating it as prose
// would just make the task list unreadable).
func successMessage(session *models.LogSession) string {
	const fallback = "Task completed successfully"
	summary := collapseWhitespace(finalAssistantText(session))
	if summary == "" || strings.HasPrefix(summary, "{") || strings.HasPrefix(summary, "[") {
		return fallback
	}
	return truncateRunes(summary, maxTerminalMessageRunes)
}

// collapseWhitespace folds newlines and runs of spaces into single spaces. A
// task list renders `result` on one line; without this, a well-structured
// multi-paragraph summary reads as a wall of run-together words.
func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// reportWorkspacePath persists the per-run workspace path (#287) on the task row
// under our held lease. It rides on a TaskStatusRunning update (the task IS
// running when the scheduled runner reports its workspace) so the atomic
// lease-checked path persists WorkspacePath without changing the lifecycle. A
// failure is logged and swallowed — the file browser is a convenience, never a
// reason to fail a run.
func (p *Pool) reportWorkspacePath(taskID uuid.UUID, path string) {
	p.reportWorkspacePathForLease(taskID, p.leaseOwner, path)
}

func (p *Pool) reportWorkspacePathForLease(taskID, leaseOwner uuid.UUID, path string) {
	if strings.TrimSpace(path) == "" {
		return
	}
	if _, err := p.store.UpdateTaskStatusAtomicWithContext(context.Background(), taskID, leaseOwner, &models.StatusUpdate{
		TaskID:        taskID,
		Status:        models.TaskStatusRunning,
		WorkspacePath: &path,
	}); err != nil {
		log.Printf("runner: failed to record workspace path for task %s: %v", taskID, err)
	}
}

// recordArtifactsForLease persists the published-artifact manifest (#204) the run's
// agent produced via publish_artifact, riding a TaskStatusRunning update under
// the held lease — BEFORE the terminal
// success clears the lease. An empty manifest persists nothing (the column
// stays NULL and GET /tasks/{id}/artifacts 404s); a persist failure is logged
// and swallowed so the run still succeeds.
func (p *Pool) recordArtifactsForLease(taskID, leaseOwner uuid.UUID, manifest json.RawMessage) {
	if len(manifest) == 0 {
		return
	}
	if _, err := p.store.UpdateTaskStatusAtomicWithContext(context.Background(), taskID, leaseOwner, &models.StatusUpdate{
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
	// The replier is wired unconditionally so the admin Notifications panel can
	// enable SMTP at runtime (#511 + notifyadmin); consult its LIVE enablement
	// here so a deployment without SMTP still does no per-run trigger-event
	// lookups (the pre-notifyadmin behavior, previously enforced by boot-time
	// nil wiring).
	if g, ok := p.emailReplier.(interface{ ReplyEnabled() bool }); ok && !g.ReplyEnabled() {
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
	// Rune-boundary clamp (#595): a byte slice here could split a multi-byte
	// rune and inject invalid UTF-8 into the next run's "## Previous Run".
	return truncate.Clamp(last, carryContextMaxChars, "…[truncated]")
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
//
// A renewal rejected with ErrTaskLeaseNotHeld means the row no longer carries
// this claim's token: the lease expired and recovery re-queued the task (a
// fresh attempt may already be running), or a lease-clearing transition
// (operator cancel, ask/wake park) landed. The local run's DB writes were
// already token-fenced, but its EXTERNAL side effects (emails, MCP writes,
// sandbox actions) would otherwise keep executing in parallel with the fresh
// attempt until natural completion — so cancel the run's context (#1116),
// shrinking the double-execution window to one renew interval. The cancel is
// the SNAPSHOTTED claim's own cancelCause, called unconditionally on the
// verdict: each run's context is per-claim, so cancelling the old claim's
// context can never touch a run this same pool re-claimed in the meantime —
// which is precisely why a live active-map lookup must NOT guard it (tryClaim
// overwrites the entry on a re-claim, and a guard keyed on the live map would
// skip the zombie in exactly the ordering that matters most on one box;
// tryClaim also cancels the overwritten entry itself, so this path is the
// backstop for the not-yet-re-claimed orderings). For the already-parked/
// stopped cases the cancel is an idempotent no-op — their handlers cancelled
// the context themselves.
//
// Any OTHER renewal error (DB unreachable, timeout) is deliberately only
// logged: it does not prove the lease is lost, and cancelling on a transient
// DB blip would kill healthy long runs. If the outage outlasts the lease
// window, recovery re-queues the task and the NEXT renewal gets the definite
// ErrTaskLeaseNotHeld above.
func (p *Pool) renewActiveLeases() {
	p.mu.Lock()
	claims := make(map[uuid.UUID]activeRun, len(p.active))
	for id, run := range p.active {
		claims[id] = run
	}
	p.mu.Unlock()

	for id, claim := range claims {
		_, err := p.reportStatusForLease(id, claim.token, models.TaskStatusRunning, "")
		if err == nil {
			continue
		}
		if errors.Is(err, storage.ErrTaskLeaseNotHeld) {
			log.Printf("runner: lease for task %s is no longer held by this run; cancelling it so its external side effects stop", id)
			claim.cancelCause(errTaskLeaseLost)
			continue
		}
		log.Printf("runner: lease renewal failed for task %s: %v", id, err)
	}
}

// RecoverExpiredLeases re-queues tasks whose lease expired (crash recovery);
// tasks already past their retry budget are dead-lettered instead (#1116) and
// not counted in the return. The scheduler ticker also calls this; the pool
// exposes it for tests and for startup recovery.
func (p *Pool) RecoverExpiredLeases() (int, error) {
	return p.store.RecoverExpiredLeases()
}
