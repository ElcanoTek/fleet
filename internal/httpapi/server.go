package httpapi

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/diskguard"
	"github.com/ElcanoTek/fleet/internal/hoststats"
	"github.com/ElcanoTek/fleet/internal/ratelimit"
	"github.com/ElcanoTek/fleet/internal/remotemcp"
	"github.com/ElcanoTek/fleet/internal/webpush"
)

// Server wires the agent Manager + store + shared-secret auth into an
// http.Handler that Next.js talks to.
type Server struct {
	// A failed durable outcome write must not masquerade as live execution on
	// retry. Startup recovers the persisted sentinel after this process exits.
	approvalPersistenceFailures sync.Map
	cfg                         *config.Config
	agent                       turnEngine
	store                       chatStore
	sharedToken                 string
	rate                        *ratelimit.Limiter
	// concurrent caps simultaneous in-flight turns per user. nil disables it
	// (rate limiting off, or FLEET_CHAT_RATE_LIMIT_CONCURRENT=0).
	concurrent *ratelimit.ConcurrencyLimiter
	// rateLimitHits tallies 429s by reason ("rpm"|"day"|"concurrent"). Pointer-held
	// so a Server value stays copyable. A Prometheus surface is deferred to #176.
	rateLimitHits *rateHitCounter
	// shareRL throttles the public read-only share endpoint (#226), keyed by
	// share TOKEN (not IP — the endpoint sits behind the Next proxy, so per-IP
	// would see only the proxy's address). Always on, independent of the per-user
	// rate-limit master switch, because /shared is the most exposed surface.
	shareRL *ratelimit.Limiter
	// webhookRL throttles authenticated inbound webhook triggers (#268), keyed by
	// the CONFIGURED trigger slug (a bounded set). Always on, independent of the
	// per-user rate-limit master switch, and consulted only after a request
	// authenticates so an unknown-slug probe never creates a bucket.
	webhookRL     *ratelimit.Limiter
	hasUsers      atomic.Bool
	lastUserCheck atomic.Int64
	// verifyLimit bounds pre-login password attempts (see auth_verify.go) —
	// the chat limiter can't, since it keys by the authenticated user.
	verifyLimit verifyLimiter

	// llmProvidersChanged rebuilds + swaps the manager's model-resolver routing
	// table after an admin LLM-provider edit (WithLLMProvidersChanged). nil in
	// tests/mock mode: edits persist, the table swaps on next boot.
	llmProvidersChanged func(context.Context) error

	// workspaceSettings backs the admin Features panel (WithWorkspaceSettings).
	// nil in tests/mock mode: the /admin/settings endpoints answer 501.
	workspaceSettings workspaceSettingsService

	// notifySettings backs the admin Notifications panel (WithNotifySettings).
	// nil in tests/mock mode: the /admin/notify-settings endpoints answer 501.
	notifySettings notifySettingsService

	// guardrailProbe backs the Features panel's guardrail "Test" button
	// (WithGuardrailProbe). nil in tests/mock mode: the endpoint answers 501.
	guardrailProbe func(ctx context.Context) GuardrailProbeResult

	// isMember reports whether an email may use chat — the scoped-tier
	// gate consulted by membershipMiddleware. nil in production, where it
	// falls back to store.IsUser. Tests whose subject isn't membership
	// override it to allow-all so fixtures needn't provision every email;
	// membership_test points it back at the real store.IsUser.
	isMember func(ctx context.Context, email string) (bool, error)

	// inflight tracks the currently-running turn for each conversation.
	// Lets the server keep generating after the SSE connection drops
	// (so phone-lock + long agent turns don't lose work) while still
	// honoring an explicit Stop from the client via
	// POST /conversations/{id}/cancel.
	//
	// Each entry carries a monotonic token so a turn whose handler is
	// cleaning up doesn't accidentally clobber a fresher entry that
	// another submit installed in the meantime.
	inflightMu sync.Mutex
	inflight   map[string]inflightEntry
	// sharedFilesMu serializes shared-file-library staging (upload/rename/
	// delete handlers) against the maintenance reconciler, so a Sync pass can
	// never observe — and "repair" — a mutation's intermediate on-disk state.
	// See internal/httpapi/shared_files.go.
	sharedFilesMu sync.Mutex
	// stopEpochs records the last Stop scope=all boundary per conversation
	// (#785): the input-queue acceptance counter's value when the Stop began
	// (#1477), so it compares exactly with each row's AcceptedSeq and is
	// immune to the wall clock. The queue sweep cancels the rows at or below
	// it, and a claim-limbo row at or below it can never launch after it
	// (stopGateForRow). Never pruned, for the same reason as stopSweepGens
	// below; all three maps are written together by beginStopSweep.
	stopEpochs map[string]int64
	// stopSweeps counts the Stop scope=all queue sweeps in flight per
	// conversation. While one is pending, maybeDrainQueue defers instead of
	// claiming, so the turn cancelled by that Stop cannot tail-call the drain
	// and claim the FIFO head before the sweep has cancelled it. The sweep
	// re-kicks the drain when it finishes.
	stopSweeps map[string]int
	// stopSweepGens counts the Stop scope=all sweeps begun per conversation.
	// A queue drain reads it when it decides a claimed row is post-Stop
	// (stopGateForRow) and registerTurnGated refuses when it has moved: a
	// Stop that began — and possibly finished — while the drain was still
	// preparing its turn made that row pre-Stop, and the sweep could not see
	// it because it was already claimed. Never pruned: a drain may hold a
	// captured value for as long as its (unbounded) turn preparation takes,
	// and resetting the counter under it would either refuse a valid row or,
	// after another Stop, re-admit a swept one (ABA). One integer per
	// conversation ever stopped in this process is cheap.
	stopSweepGens   map[string]uint64
	inflightCounter uint64
	// cancelledInputs records a Stop that named an input by its key
	// (POST /conversations/{id}/cancel with input_id), keyed by
	// inputKeyMark, with the time it was recorded. registerTurnGated refuses
	// to launch a turn for a marked key, under the same inflightMu section
	// that records the mark and cancels an already-registered turn for it —
	// so a key's turn is either cancelled or never launched, whichever side
	// of registration the Stop lands on. Pruned after cancelledInputTTL.
	cancelledInputs map[string]time.Time
	// inputKeyLocks serializes the first submissions of one (user, key)
	// (recoverFirstSubmission).
	inputKeyLocks keyedLocks

	// clientConfig is the loaded client bundle that backs GET /client-config
	// (branding + empty-state). nil in tests / mock mode that don't supply one;
	// the endpoint then returns neutral generic defaults.
	clientConfig *clientconfig.Bundle

	// remoteMCP serves the per-user remote (hosted) MCP + OAuth endpoints (#443).
	// nil when the feature is unconfigured (no encryption key / public base URL),
	// in which case the endpoints return a clear "not configured" error.
	remoteMCP *remotemcp.Service

	// push is the browser Web Push sender (#292). nil when the feature is
	// unconfigured (no VAPID keys), in which case the /push/* endpoints return
	// 501 and the approval-staged push trigger is a no-op.
	push *webpush.Service

	// sessionApprovals holds per-conversation batch pre-approvals (#300): in-memory,
	// session-scoped policies consulted by approvalStager.Stage before staging a card.
	sessionApprovals *SessionApprovalRegistry

	// sseReconnects tallies SSE reconnect outcomes (within_buffer | db_fallback |
	// buffer_expired | no_content). Behind a pointer so a Server value stays
	// copyable; a Prometheus surface is deferred to the metrics issue (#176).
	sseReconnects *reconnectCounter

	// shuttingDown is set by BeginShutdown when the process starts draining: new
	// POST /chat are rejected with 503 and /healthz reports 503 so a load balancer
	// stops routing here. Detached turns already in flight keep running until the
	// shutdown path drains them (DrainTurns) or force-cancels them.
	shuttingDown atomic.Bool

	// activeTurns tracks detached runTurnAsync goroutines so the shutdown path can
	// block on them (DrainTurns). activeTurnCount mirrors it for the SIGUSR1
	// diagnostic (sync.WaitGroup exposes no count). Both are incremented together,
	// before the goroutine launches, so Wait never races ahead of an Add.
	activeTurns     sync.WaitGroup
	activeTurnCount atomic.Int64

	// background tracks detached work that is NOT a turn — the queue-drain
	// re-kick, memory-graph extraction, retained-buffer eviction, approval push
	// sends. activeTurns never covered those, so nothing waited for them and they
	// outlived both shutdown and a test's store. See background.go. Value, not
	// pointer: tests build Server as a struct literal and a nil field would panic.
	background backgroundTracker

	// Health-summary inputs (#301). startTime backs uptime; version is the build
	// label; workerStats (optional) returns scheduler worker/task counts from the
	// sched store — injected so httpapi stays sched-agnostic. nil → that section
	// is reported null.
	startTime   time.Time
	version     string
	workerStats func(context.Context) (*WorkerStats, error)
	// hostStats is the dependency-free procfs/statfs collector behind the
	// admin-only Server settings page. It retains only the previous counters
	// needed to calculate CPU/network rates.
	hostStats *hoststats.Collector
	// doctorMu serializes /admin/doctor runs: the deep mode launches a
	// throwaway sandbox container, and N admins clicking "run checks" at once
	// must not stampede podman.
	doctorMu sync.Mutex

	// diskGuard measures free space on the data dir's filesystem and decides
	// whether the box should shed scheduled work. Read-only here: the guard is
	// consulted for /healthz and the admin health summary, and it is the
	// SCHEDULER (internal/runner) that acts on the decision. nil = unwired.
	diskGuard *diskguard.Guard

	// lastMaintenance is the UnixNano instant the reclamation pass last ran,
	// from EITHER driver (cmd/fleet's ticker via NoteMaintenanceRun, or a
	// post-turn pass). The post-turn path compare-and-swaps it so concurrent
	// turns cannot stampede the global sweeps. Zero = never. See maintenance.go.
	lastMaintenance atomic.Int64

	// memoryGraphExtractor mines a memory for knowledge-graph triples (#523).
	// Injected via WithMemoryGraphExtractor so httpapi depends on the seam, not
	// on wiring; nil OR cfg.MemoryGraphEnabled=false disables extraction and
	// leaves every memory path byte-for-byte unchanged.
	memoryGraphExtractor MemoryGraphExtractor

	// scheduleTask creates a scheduled task in the orchestrator on behalf of an
	// approved interactive schedule_task call (#239). Injected so httpapi stays
	// sched-agnostic — main.go translates TaskScheduleRequest to the sched model
	// and calls the storage create path. nil → schedule_task approvals report the
	// feature is unconfigured (no task is created).
	scheduleTask func(context.Context, TaskScheduleRequest) (*TaskScheduleResult, error)

	// manageTasks updates or stops EXISTING orchestrator tasks on behalf of an
	// approved chat manage_tasks call (#1152). Same seam discipline as
	// scheduleTask: httpapi stays sched-agnostic and main.go supplies the
	// adapter. nil = the capability is unconfigured and the tool reports so.
	manageTasks func(context.Context, TaskMutationRequest) (*TaskMutationResult, error)

	// opsAdmins grants/revokes/lists Operations-Center admin rows by email so
	// the admin Users tab carries `fleet admin add`'s two-plane semantics.
	// Injected (WithOpsAdmins) so httpapi stays sched-agnostic; nil → role
	// writes touch the chat plane only and the list reports no annotation.
	opsAdmins OpsAdmins
}

// TaskScheduleRequest is the sched-agnostic payload the chat approval path hands
// to the injected scheduler seam (#239). main.go maps it to models.TaskCreate.
// Exactly one of Cron / RunAt should be set (or neither, for run-immediately);
// the caller validates that before staging.
type TaskScheduleRequest struct {
	// Title is the operator-facing label confirmed in chat. It maps to
	// TaskCreate.Title, not TaskCreate.Name: Name is the unique portable
	// definition key and is not rendered in the Operations Center.
	Title         string
	Prompt        string
	Model         string
	Cron          string
	RunAt         *time.Time
	MaxIterations int
	AllowNetwork  bool
	// ThinkingBudgetTokens is the per-task extended-thinking override (#220):
	// nil = inherit the deployment default, 0 = off, >0 = this task's budget.
	ThinkingBudgetTokens *int
	Tags                 []string
	// Connectors is the conversation's connector selection the task inherits
	// (ADR-0068); it maps to TaskCreate.MCPSelection. Empty creates a task
	// that binds only the bundle's always-on servers, exactly as before.
	Connectors []TaskConnector
	// RequestedBy is the approving chat user's email — the principal the
	// per-user rolling budget gate (#601 part 2) checks before the task is
	// created, so scheduling from chat cannot bypass a budget that would refuse
	// the same user on POST /tasks. Identity only; the created task's
	// provenance still comes from the source-chat tag (the seam does not
	// resolve chat emails to sched users).
	RequestedBy string
}

// TaskMutationRequest is the sched-agnostic payload the approved manage_tasks
// path hands to the orchestrator. Exactly one of TaskIDs / Match names the
// targets; the change fields are all "omitted = leave alone".
type TaskMutationRequest struct {
	// Action is "update" or "stop" (tools.ManageTasksAction*).
	Action  string
	TaskIDs []string
	// Match selects by property. Only ever set for "update" — the tool refuses
	// a filtered stop, because stopping cannot be undone from chat and a filter
	// the human has not seen resolved is not something to approve.
	Match *TaskMutationMatch

	Prompt        string
	Cron          string
	Model         string
	MaxIterations int
	AddTags       []string
	RemoveTags    []string

	// RequestedBy is the approving chat user's email — the principal the
	// adapter authorizes against, so chat cannot edit a task the same person
	// could not edit through the API.
	RequestedBy string
}

// TaskMutationMatch is the property selector behind a bulk update.
type TaskMutationMatch struct {
	Query string
	Tag   string
	Model string
}

// TaskMutationResult reports what an approved manage_tasks call actually did,
// per task. The list is the point: a bulk edit that reports only a count is a
// bulk edit nobody can check.
type TaskMutationResult struct {
	Changed []TaskMutationEntry
	// Skipped carries targets the adapter declined, with the reason — a task
	// that no longer exists, one the approver does not own, one already
	// terminal. Reported rather than swallowed.
	Skipped []TaskMutationEntry
	// Matched is how many tasks the selector found, which can exceed
	// len(Changed) only when something was skipped.
	Matched int
}

// TaskMutationEntry names one affected task for the chat-visible report.
type TaskMutationEntry struct {
	ID     string
	Label  string
	Detail string
}

// TaskScheduleResult is what the scheduler seam returns after creating a task.
type TaskScheduleResult struct {
	ID      string
	Status  string
	NextRun time.Time // zero = runs as soon as a worker is free
}

// reconnectCounter is a concurrency-safe tally of SSE reconnect outcomes.
type reconnectCounter struct {
	mu     sync.Mutex
	counts map[string]int64
}

func newReconnectCounter() *reconnectCounter {
	return &reconnectCounter{counts: map[string]int64{}}
}

func (c *reconnectCounter) inc(outcome string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.counts[outcome]++
	c.mu.Unlock()
}

func (c *reconnectCounter) snapshot() map[string]int64 {
	if c == nil {
		return map[string]int64{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int64, len(c.counts))
	for k, v := range c.counts {
		out[k] = v
	}
	return out
}

// SSEReconnectCounts returns a snapshot of SSE reconnect outcomes by label.
func (s *Server) SSEReconnectCounts() map[string]int64 { return s.sseReconnects.snapshot() }

// ── graceful shutdown (#278) ───────────────────────────────────────────────

// BeginShutdown marks the server as draining: new POST /chat are rejected with
// 503 and /healthz reports 503 so a load balancer / readiness probe stops
// routing new requests here. It also sends a one-shot `shutdown` SSE control
// frame to every live stream subscriber so an attached client reconnects to a
// healthy instance rather than waiting out the drain. Idempotent — only the
// first call broadcasts.
func (s *Server) BeginShutdown() {
	if s.shuttingDown.Swap(true) {
		return
	}
	s.broadcastShutdownFrame()
}

// IsDraining reports whether graceful shutdown has begun (#215/#278). The
// readiness probe consults this so a draining instance reports not_ready/503 —
// matching /healthz — and load balancers stop routing new traffic to it.
func (s *Server) IsDraining() bool { return s.shuttingDown.Load() }

// broadcastShutdownFrame emits a transient `shutdown` SSE frame to every running
// turn's live subscribers. Buffers are collected under inflightMu, then notified
// outside it, so we never hold inflightMu while taking a buffer's own lock
// (lock-ordering hygiene).
func (s *Server) broadcastShutdownFrame() {
	s.inflightMu.Lock()
	bufs := make([]*turnBuffer, 0, len(s.inflight))
	for _, e := range s.inflight {
		if e.buf != nil && e.IsRunning() {
			bufs = append(bufs, e.buf)
		}
	}
	s.inflightMu.Unlock()
	for _, b := range bufs {
		b.broadcastControl("shutdown")
	}
}

// DrainTurns blocks until all in-flight (detached) turn goroutines finish or ctx
// (the shutdown grace deadline) fires. It returns true if the turns drained in
// time, false if ctx expired first — the caller then force-cancels via
// CancelInflightTurns.
func (s *Server) DrainTurns(ctx context.Context) bool {
	done := make(chan struct{})
	go func() {
		s.activeTurns.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// CancelInflightTurns fires the cancel func of every still-running turn so a
// grace-expired shutdown stops detached turn goroutines: each turnCtx is
// cancelled and agentcore exits at its next checkpoint. Returns the number of
// turns cancelled.
func (s *Server) CancelInflightTurns() int {
	s.inflightMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.inflight))
	for _, e := range s.inflight {
		if e.IsRunning() {
			cancels = append(cancels, e.cancel)
		}
	}
	s.inflightMu.Unlock()
	for _, c := range cancels {
		c()
	}
	return len(cancels)
}

// ActiveTurns reports the number of in-flight detached turn goroutines — the
// diagnostic counter behind the SIGUSR1 status log.
func (s *Server) ActiveTurns() int { return int(s.activeTurnCount.Load()) }

// releasePersistentSandbox tears down the persistent per-conversation
// run_python sandbox (#213) for convID after its conversation is deleted, so
// the kernel + container are reclaimed promptly instead of waiting for the idle
// reaper. A no-op when persistent mode is off, the engine is absent (mock/test
// setups), or the conversation never had a persistent sandbox — every layer
// down to Pool.ReleaseChatSession is nil-safe. If a turn is still in flight, the
// pool defers the actual close to the turn's last sandbox borrow.
func (s *Server) releasePersistentSandbox(convID string) {
	if s.agent == nil || convID == "" {
		return
	}
	s.agent.SandboxPool().ReleaseChatSession(convID)
}

// Option customizes a Server at construction.
type Option func(*Server)

// WithClientConfig wires the loaded client bundle so GET /client-config can
// serve the deployment's branding + chat empty-state to the web.
func WithClientConfig(b *clientconfig.Bundle) Option {
	return func(s *Server) { s.clientConfig = b }
}

// WithStartTime records the process start so the health summary (#301) can
// report uptime.
// WithRemoteMCP injects the per-user remote-MCP + OAuth service (#443). Omit it
// (or pass nil) to leave the feature off.
func WithRemoteMCP(svc *remotemcp.Service) Option {
	return func(s *Server) { s.remoteMCP = svc }
}

// WithLLMProvidersChanged wires the callback the admin LLM-provider endpoints
// invoke after persisting a change: cmd/fleet points it at "re-read the store,
// merge with the bundle table, swap the manager's resolver". Omitted (tests,
// mock mode), edits persist without a live routing-table swap.
func WithLLMProvidersChanged(fn func(context.Context) error) Option {
	return func(s *Server) { s.llmProvidersChanged = fn }
}

// WithPush injects the browser Web Push sender (#292). Omit it (or pass nil)
// to leave the feature off: the /push/* endpoints then answer 501.
func WithPush(svc *webpush.Service) Option {
	return func(s *Server) { s.push = svc }
}

func WithStartTime(t time.Time) Option {
	return func(s *Server) { s.startTime = t }
}

// WithVersion sets the build label reported by the health summary (#301).
func WithVersion(v string) Option {
	return func(s *Server) { s.version = v }
}

// WithDiskGuard injects the host disk headroom guard (internal/diskguard) so
// /healthz can report a box that has started shedding scheduled work and the
// admin health summary can show the numbers behind that decision.
//
// Interactive chat is deliberately NOT gated on it — a full disk is nearly
// always produced by unattended runs, and chat is the interface an operator
// uses to fix it. /healthz reports degraded so a monitor pages someone; it does
// not stop serving. nil leaves the disk section absent and /healthz unchanged.
func WithDiskGuard(g *diskguard.Guard) Option {
	return func(s *Server) { s.diskGuard = g }
}

// WithWorkerStats injects a provider for scheduler worker/task counts (from the
// sched store) so the health summary (#301) can include them without httpapi
// importing the sched packages. nil leaves the workers/tasks section null.
func WithWorkerStats(fn func(context.Context) (*WorkerStats, error)) Option {
	return func(s *Server) { s.workerStats = fn }
}

// WithTaskScheduler injects the orchestrator task-create seam used to resolve an
// approved interactive schedule_task call (#239), so httpapi can create a
// scheduled task without importing the sched packages. nil leaves schedule_task
// approvals reporting the feature unconfigured.
func WithTaskScheduler(fn func(context.Context, TaskScheduleRequest) (*TaskScheduleResult, error)) Option {
	return func(s *Server) { s.scheduleTask = fn }
}

// WithTaskManager injects the orchestrator update/stop seam used to resolve an
// approved manage_tasks call (#1152). Unset leaves the capability off: the tool
// still stages a card, and approving it reports that the deployment has no
// orchestrator wired rather than silently doing nothing.
func WithTaskManager(fn func(context.Context, TaskMutationRequest) (*TaskMutationResult, error)) Option {
	return func(s *Server) { s.manageTasks = fn }
}

// inflightEntry pairs the cancel-func for a turn with a unique token,
// the per-turn event buffer, and a finishedAt timestamp.
//
// Two-phase lifecycle:
//   - while running: buf accepts Emit calls and fans events out to any
//     live Attach subscribers. finishedAt is zero.
//   - after Finish: buf is sealed but kept in the map for
//     bufferRetainTTL so a client reconnecting within that window still
//     sees the full replay. finishedAt is set; eventual eviction is
//     scheduled by a timer goroutine.
type inflightEntry struct {
	cancel     context.CancelFunc
	token      uint64
	buf        *turnBuffer
	turnID     string
	finishedAt time.Time
	// steer is the running turn's mid-turn input mailbox (#785); nil for
	// turns launched before a steer could exist (mock mode, tests).
	steer *steerMailbox
	// submissionID is the client-minted id of the POST /chat submission this
	// turn was started FOR (#1592), echoed back by /inflight. A browser whose
	// submission acknowledgement was lost in transit cannot otherwise tell the
	// turn the server started for it from one that was already running: the
	// turn id is new either way, and the transcript is inconclusive because a
	// turn is registered before its user message commits. Empty for turns no
	// submission named (webhooks, scheduled runs, pre-#1592 clients), which
	// reads as "no evidence" rather than "not yours".
	submissionID string
	// inputKey is the idempotency key (client_input_id) of the input this
	// turn runs, "" when none: a Stop that names the key cancels this turn.
	inputKey string
}

// IsRunning reports whether the turn is still generating (buffer open).
// finishedAt flips shortly after the buffer seals, so the sealed state is
// consulted too — otherwise a submission racing the previous turn's last
// bookkeeping microseconds would queue (#785) instead of running directly.
func (e inflightEntry) IsRunning() bool {
	if !e.finishedAt.IsZero() {
		return false
	}
	if e.buf != nil && e.buf.Sealed() {
		return false
	}
	return true
}

// stoppable reports whether a Stop can still change how the turn ends: it
// is running and has not emitted a terminal frame. A turn that already
// ended — turn.error or turn.model_required included — while post-turn work
// keeps its buffer unsealed ended on its own, so a cancel is a no-op there
// and a Stop must report it finished rather than take its terminal frame
// for the stop's.
func (e inflightEntry) stoppable() bool {
	if !e.IsRunning() {
		return false
	}
	if e.buf != nil {
		if e.buf.terminalOutcome() != "" {
			return false
		}
	}
	return true
}

// New wires a Server. Call Routes() to get the http.Handler.
//
// mgr is the interactive agent engine (the turnEngine contract). It may be nil
// in mock mode and in tests that exercise only the DB-backed, mock-turn, or
// auth paths — the live turn path short-circuits before touching it. cmd/fleet
// (P6b) supplies the concrete engine implementation.
func New(cfg *config.Config, mgr turnEngine, st chatStore, opts ...Option) *Server {
	s := &Server{
		cfg:              cfg,
		agent:            mgr,
		store:            st,
		sharedToken:      cfg.SharedToken,
		rateLimitHits:    newRateHitCounter(),
		inflight:         make(map[string]inflightEntry),
		sessionApprovals: NewSessionApprovalRegistry(),
		sseReconnects:    newReconnectCounter(),
		hostStats:        hoststats.New(),
		// Per-token cap on the public /shared read endpoint (#226): generous for
		// real viewers, a hard ceiling against scraping/DDoS amplification of a
		// single link. No daily window.
		shareRL: ratelimit.New(sharedReadsPerMinutePerToken, 0),
		// Per-slug cap on authenticated inbound webhook triggers (#268). No daily
		// window. Overridable via FLEET_WEBHOOK_RATE_LIMIT_PER_MINUTE.
		webhookRL: ratelimit.New(envInt("FLEET_WEBHOOK_RATE_LIMIT_PER_MINUTE", webhookTriggersPerMinutePerSlug), 0),
	}
	// Rate limiting is a single master switch. When on, the RPM/day window and
	// the per-user concurrent-turn cap are both live; when off, both limiters are
	// nil and their (nil-safe) checks pass through.
	if cfg.RateLimitEnabled {
		s.rate = ratelimit.New(cfg.RatePerMinute, cfg.RatePerDay)
		s.concurrent = ratelimit.NewConcurrencyLimiter(cfg.RateLimitConcurrent)
	}
	for _, opt := range opts {
		opt(s)
	}
	log.Printf("sse: buffer_duration=%s max_bytes_per_turn=%d heartbeat_interval=%s",
		bufferRetainTTL, sseMaxBytesPerTurn, sseHeartbeatInterval)
	// Surface the active IP access-control state (#314) so an operator can confirm
	// their config loaded. Silent when neither list is set (the default open case).
	logIPFilter(ipFilterConfig{allow: cfg.IPAllowlist, deny: cfg.IPDenylist, trustedProxies: cfg.TrustedProxies})
	return s
}

// defaultTurnExecutionTimeout caps how long a single turn may run
// server-side once detached from the SSE connection. Well above the
// per-turn cost + iteration ceilings, which are the real safety nets.
// Operators can override via CHAT_TURN_TIMEOUT_SECONDS (config.TurnTimeoutSeconds).
const defaultTurnExecutionTimeout = 30 * time.Minute

// turnTimeout resolves the configured per-turn wall-clock cap, falling
// back to defaultTurnExecutionTimeout when unset or non-positive.
func (s *Server) turnTimeout() time.Duration {
	if s.cfg != nil && s.cfg.TurnTimeoutSeconds > 0 {
		return time.Duration(s.cfg.TurnTimeoutSeconds) * time.Second
	}
	return defaultTurnExecutionTimeout
}

// bufferRetainTTL is how long a finished turn's event buffer stays in the
// inflight map after completion — long enough for a client that dropped
// mid-turn to return and replay from its Last-Event-ID. Default 15m covers the
// p95 long interactive agent run; tune via FLEET_SSE_BUFFER_DURATION (e.g. 2m on
// a memory-constrained box, 30m on beefy hardware).
var bufferRetainTTL = envDuration("FLEET_SSE_BUFFER_DURATION", 15*time.Minute)

// sseMaxBytesPerTurn caps a single turn's in-memory event buffer. Past it, the
// oldest events are evicted (sliding window) so a chatty 15m turn can't grow
// unbounded; a reconnecting client past the slide gets a `reconnect` event
// describing the gap. 0 = unlimited. FLEET_SSE_BUFFER_MAX_BYTES_PER_TURN.
var sseMaxBytesPerTurn = envInt("FLEET_SSE_BUFFER_MAX_BYTES_PER_TURN", 5<<20)

// sseHeartbeatInterval is the idle keepalive cadence on an attached SSE stream,
// keeping the TCP socket alive through mobile OS connection managers and proxy
// read timeouts during quiet stretches (the model reasoning with no tool
// output). 0 disables. FLEET_SSE_HEARTBEAT_INTERVAL.
var sseHeartbeatInterval = envDuration("FLEET_SSE_HEARTBEAT_INTERVAL", 15*time.Second)

// envDuration reads a time.ParseDuration-formatted env var, falling back to def
// when unset or unparseable.
func envDuration(key string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		log.Printf("config: %s is not a valid duration; using default %s", key, def)
	}
	return def
}

// envInt reads an integer env var, falling back to def when unset or unparseable.
func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
		log.Printf("config: %s is not a valid integer; using default %d", key, def)
	}
	return def
}

// registerTurn installs a fresh turnBuffer + cancel entry for convID IF no
// turn is currently running. A running entry is never cancelled here (#785) —
// a second submission is not an abandonment signal; it queues, and explicit
// /cancel stays the only Stop. ok=false reports a running turn (the caller
// falls back to the queue path). A RETAINED finished entry is still evicted +
// sealed exactly as before. Returns the buffer, the turn ID, and a token that
// finishTurn must present so stale finishers can't clobber a fresher entry.
//
// This ungated form serves turns with no steer mailbox and no queue row
// (webhooks); chat submissions and queue drains go through registerTurnGated.
func (s *Server) registerTurn(convID string, cancel context.CancelFunc) (*turnBuffer, string, uint64, bool) {
	buf, turnID, token, ok, _ := s.registerTurnGated(convID, cancel, nil, nil, "")
	return buf, turnID, token, ok
}

// registerTurnGated is registerTurn with an optional Stop gate for the
// queue-drain path (queued != nil). The registration is refused — under the
// same inflightMu beginStopSweep takes, so the check is atomic with the claim
// becoming a running turn — when a Stop scope=all has begun since the drain
// decided its row was post-Stop (stopGateForRow read queued.sweepGen in the
// same section as the Stop instant). That closes the window between the
// drain's decision and its registration: a Stop landing there makes the row
// pre-Stop, but the sweep cannot see it because it is already claimed, and
// the drain may reach this point while the sweep is still running or long
// after it finished (turn preparation loads the conversation, history and
// memories first). A sweep in flight whose Stop the drain already compared
// against does not hold the row back: that sweep's set never contained it.
// swept reports a refusal so the caller cancels the row instead of
// un-claiming it.
func (s *Server) registerTurnGated(convID string, cancel context.CancelFunc, steer *steerMailbox, queued *queuedLaunch, submissionID string) (buf *turnBuffer, turnID string, token uint64, ok, swept bool) {
	s.inflightMu.Lock()
	if queued != nil && s.stopSweepGens[convID] != queued.sweepGen {
		s.inflightMu.Unlock()
		return nil, "", 0, false, true
	}
	inputKey := ""
	if queued != nil {
		inputKey = queued.inputKey
	}
	if inputKey != "" {
		if at, marked := s.cancelledInputs[inputKeyMark(convID, inputKey)]; marked {
			delete(s.cancelledInputs, inputKeyMark(convID, inputKey))
			if time.Since(at) <= cancelledInputTTL {
				s.inflightMu.Unlock()
				return nil, "", 0, false, true
			}
		}
	}
	prev, hadPrev := s.inflight[convID]
	if hadPrev && prev.IsRunning() {
		s.inflightMu.Unlock()
		return nil, "", 0, false, false
	}
	if hadPrev {
		delete(s.inflight, convID)
	}
	s.inflightCounter++
	token = s.inflightCounter
	turnID = uuid.NewString()
	buf = newTurnBuffer(convID, turnID)
	s.inflight[convID] = inflightEntry{
		cancel:       cancel,
		token:        token,
		buf:          buf,
		turnID:       turnID,
		steer:        steer,
		submissionID: submissionID,
		inputKey:     inputKey,
	}
	s.inflightMu.Unlock()

	// Seal the evicted buffer after releasing inflightMu: Finish drains
	// the persister (DB writes with multi-second budgets), and holding
	// the server-wide lock across that would stall every conversation's
	// /chat, /stream and /cancel behind one slow Postgres round-trip.
	// The entry is already out of the map, so nobody else can reach it;
	// Finish is idempotent against the evicted turn's own deferred
	// finishTurn.
	if hadPrev && prev.buf != nil {
		prev.buf.Finish()
	}
	return buf, turnID, token, true, false
}

// finishTurn seals the buffer and marks the entry finished, keeping it
// retained for bufferRetainTTL so a late reconnect still sees replay.
// A stale token (a newer turn already replaced us) makes this a no-op
// so we don't evict the fresher entry.
func (s *Server) finishTurn(convID string, token uint64) {
	// Snapshot the buffer under the lock, but seal it after releasing:
	// Finish waits out the persister drain + a FinishTurn UPDATE (5s DB
	// budgets each), and holding the server-wide inflightMu across that
	// would block every conversation on one slow Postgres round-trip.
	s.inflightMu.Lock()
	cur, ok := s.inflight[convID]
	if !ok || cur.token != token {
		s.inflightMu.Unlock()
		return
	}
	buf := cur.buf
	s.inflightMu.Unlock()

	// Seal first, then flip finishedAt, so "finished" always implies
	// "buffer sealed" for /inflight pollers and late Attach calls.
	if buf != nil {
		buf.Finish()
	}

	s.inflightMu.Lock()
	cur, ok = s.inflight[convID]
	if !ok || cur.token != token {
		// A newer turn replaced us while we were sealing; it owns the
		// entry now. Our buffer is sealed either way.
		s.inflightMu.Unlock()
		return
	}
	cur.finishedAt = time.Now()
	s.inflight[convID] = cur
	s.inflightMu.Unlock()

	// Evict the retained buffer after TTL. If another turn has replaced
	// this one by then (token mismatch), leave it alone.
	s.background.After("httpapi.buffer_retain_evict", bufferRetainTTL, func() {
		s.inflightMu.Lock()
		defer s.inflightMu.Unlock()
		if cur, ok := s.inflight[convID]; ok && cur.token == token {
			delete(s.inflight, convID)
		}
	})
}

// cancelInflight cancels the currently-running turn for convID, if any.
// Returns true if a cancellation was issued (i.e. the turn was still
// running — a cancel against an already-finished buffer is a no-op).
func (s *Server) cancelInflight(convID string) bool {
	s.inflightMu.Lock()
	entry, ok := s.inflight[convID]
	s.inflightMu.Unlock()
	if !ok || !entry.IsRunning() {
		return false
	}
	entry.cancel()
	return true
}

// cancelInflightTurn cancels convID's running turn only if it is turnID. The
// check and the cancel read one snapshot taken under inflightMu, and a turn id
// is never reused, so a successor registered after turnID ended is never hit.
func (s *Server) cancelInflightTurn(convID, turnID string) turnStop {
	return s.cancelSteerTurn(convID, turnID, "")
}

// cancelSteerTurn is cancelInflightTurn for a Stop by key of the injected
// steer steerRowID ("" for none) that turnID carries. When it cancels the
// turn it first records the steer on the turn's buffer, so the settlement
// (which runs after the turn ends) cancels an uncommitted steer instead of
// returning it to the queue, even when the Stop is answered before the turn
// confirms it — or after the turn already ended on its own but has not
// settled, since a Stop of the steer must not leave it to be re-queued. A
// turn whose buffer sealed is not flagged: its settlement is its own.
func (s *Server) cancelSteerTurn(convID, turnID, steerRowID string) turnStop {
	s.inflightMu.Lock()
	entry, ok := s.inflight[convID]
	s.inflightMu.Unlock()
	if !ok || entry.turnID != turnID || !entry.IsRunning() {
		return turnNotStopped
	}
	if steerRowID != "" && entry.buf != nil {
		entry.buf.addStoppedSteer(steerRowID)
	}
	if !entry.stoppable() {
		return turnNotStopped // it already ended: the cancel would stop nothing
	}
	entry.cancel()
	return entry.confirmStopped()
}

// turnStop is what a Stop did to a turn, as its own terminal frame shows.
type turnStop int

const (
	// turnNotStopped: the turn was not running, or it ended on its own —
	// completed, or failed — in the instant between the running check and
	// the cancel.
	turnNotStopped turnStop = iota
	// turnStopped: the turn's terminal frame after the cancel is
	// turn.cancelled.
	turnStopped
	// turnStopUnconfirmed: the cancel was sent but no terminal frame
	// arrived within stopConfirmWait. It is not claimed either way.
	turnStopUnconfirmed
)

// stopConfirmWait bounds how long a Stop waits for the turn it cancelled to
// emit its terminal frame. A var so tests can shorten it.
var stopConfirmWait = 3 * time.Second

// confirmStopped reads what a cancel did. "Running" is checked before the
// cancel and the turn can finish in between, which makes the cancel a no-op,
// so only the turn's terminal frame decides — read as soon as it is emitted,
// not when the buffer seals (post-turn work such as auto-titling runs before
// the seal). No frame by the deadline is unconfirmed, never assumed stopped.
func (e inflightEntry) confirmStopped() turnStop {
	if e.buf == nil {
		return turnStopUnconfirmed
	}
	deadline := time.Now().Add(stopConfirmWait)
	for {
		switch e.buf.terminalOutcome() {
		case "turn.cancelled":
			return turnStopped
		case "turn.completed", "turn.error", "turn.model_required":
			// Completed, or failed on its own in the instant between the
			// stoppable check and the cancel: the cancel stopped nothing. A
			// failure the cancel itself caused is advertised turn.cancelled
			// (runTurnAsync), so only a failure of the turn's own is here.
			return turnNotStopped
		}
		if time.Now().After(deadline) {
			return turnStopUnconfirmed
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// cancelledInputTTL bounds how long a Stop by input key is remembered for a
// turn that has not registered yet.
const cancelledInputTTL = 10 * time.Minute

// maxCancelledInputs bounds the Stop-by-key marks held at once.
const maxCancelledInputs = 4096

func inputKeyMark(convID, key string) string { return convID + "\x00" + key }

// cancelInputTurn stops the turn for the input with idempotency key key, on
// whichever side of registration it is: a registered turn for the key is
// cancelled, and the key is marked so a turn not registered yet is refused
// by registerTurnGated. Both happen in one inflightMu section, the one
// registration takes, so no launch can slip between them.
func (s *Server) cancelInputTurn(convID, key string) turnStop {
	now := time.Now()
	s.inflightMu.Lock()
	if s.cancelledInputs == nil {
		s.cancelledInputs = make(map[string]time.Time)
	}
	for k, at := range s.cancelledInputs {
		if now.Sub(at) > cancelledInputTTL {
			delete(s.cancelledInputs, k)
		}
	}
	if len(s.cancelledInputs) >= maxCancelledInputs {
		// Full of live marks: drop the oldest, so the set stays bounded
		// however many distinct keys are stopped.
		oldest, oldestAt := "", now
		for k, at := range s.cancelledInputs {
			if at.Before(oldestAt) {
				oldest, oldestAt = k, at
			}
		}
		delete(s.cancelledInputs, oldest)
	}
	entry, ok := s.inflight[convID]
	running := ok && entry.IsRunning() && entry.inputKey == key
	if !running {
		s.cancelledInputs[inputKeyMark(convID, key)] = now
	}
	s.inflightMu.Unlock()
	if !running {
		return turnNotStopped
	}
	if entry.buf != nil {
		// Before the cancel: the settlement reads it after the turn ends.
		// Set even for a turn that already ended on its own (a failure
		// before its user entry committed), so its settlement does not
		// re-queue the input the Stop named for an unattended re-run.
		entry.buf.stoppedByKey.Store(true)
	}
	if !entry.stoppable() {
		return turnNotStopped // it already ended: the cancel would stop nothing
	}
	entry.cancel()
	return entry.confirmStopped()
}

// inputKeyStopped reports whether a Stop naming key is still in force.
func (s *Server) inputKeyStopped(convID, key string) bool {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	at, ok := s.cancelledInputs[inputKeyMark(convID, key)]
	return ok && time.Since(at) <= cancelledInputTTL
}

// clearInputKeyMark drops a Stop-by-key mark once the database shows the
// input had already finished: nothing is left for it to refuse, and left in
// place it would cancel a later, legitimate reuse of the key (after the
// finished row is purged) for the rest of its TTL.
func (s *Server) clearInputKeyMark(convID, key string) {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	delete(s.cancelledInputs, inputKeyMark(convID, key))
}

// getInflight returns a snapshot of the current entry for convID.
func (s *Server) getInflight(convID string) (inflightEntry, bool) {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	entry, ok := s.inflight[convID]
	return entry, ok
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	// Draining (#278): report 503 the moment graceful shutdown begins so a load
	// balancer / readiness probe stops routing new traffic to this instance while
	// it drains in-flight work. Checked first — a draining box is unready
	// regardless of provider health.
	if s.shuttingDown.Load() {
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]any{"status": "shutting_down"})
		return
	}
	// Surface LLM provider degradation (#267): if any model's circuit is open
	// (sustained recent failures), report degraded so an operator/monitor sees it.
	// Half-open (recovering) and closed are healthy.
	if s.agent != nil {
		for _, h := range s.agent.ProviderHealth() {
			if h.State == agentcore.CircuitOpen.String() {
				writeJSONStatus(w, http.StatusServiceUnavailable, map[string]any{
					"status": "degraded",
					"reason": "llm provider circuit open",
					"model":  h.Slug,
				})
				return
			}
		}
	}
	// Disk backpressure is deliberately NOT reported here. /healthz is the
	// load-balancer gate — a 503 drains chat traffic off this box — and a
	// shedding box is one whose CHAT is fine and whose scheduled queue has
	// paused. Draining it would remove the very interface an operator needs to
	// reclaim the space, which is the opposite of what the guard is for. The
	// signal lives where a non-critical degradation belongs: /readyz reports
	// `disk` degraded (207, not 503), fleet_disk_shedding is the alert, and the
	// admin health summary carries the numbers. See internal/diskguard.
	_, _ = w.Write([]byte("ok"))
}

// ── helpers ────────────────────────────────────────────────────────────────

// exportFilename builds a safe, recognizable filename for a download:
// slugified title + short id + extension. Keeps the Save dialog
// self-explanatory without trusting user-chosen characters in the
// Content-Disposition header — every rune outside [A-Za-z0-9 _-] is
// dropped, so a quote can never terminate the header's quoted string.
//
// fallback names the thing being exported when the title slugifies to
// nothing (empty, or all-punctuation like `///"""`). It is a parameter
// rather than a constant because the callers export different nouns: a
// project export landing as "chat-a1b2c3d4.json" reads as a bug.
func exportFilename(title, id, ext, fallback string) string {
	slug := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == ' ', r == '-', r == '_':
			return '-'
		}
		return -1
	}, title)
	slug = strings.Trim(strings.ReplaceAll(slug, "--", "-"), "-")
	if len(slug) > 50 {
		slug = slug[:50]
	}
	if slug == "" {
		slug = fallback
	}
	shortID := id
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	return slug + "-" + shortID + "." + ext
}

// setJSONContentType stamps the JSON media type unless the handler already
// chose one. A few responses carry an explicit charset or were built around a
// different type, and silently clobbering that would change the response —
// so the helpers default the header rather than owning it.
func setJSONContentType(w http.ResponseWriter) {
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
}

// writeJSON encodes v as the response body. The status line and headers are
// already committed by the time Encode runs, so a mid-body failure (typically
// the client disconnecting) cannot change the response — log it for
// diagnostics rather than swallowing it silently.
func writeJSON(w http.ResponseWriter, v any) {
	setJSONContentType(w)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write json: %v", err)
	}
}

// keyedLocks is a set of mutexes by key, each dropped once nobody holds or
// waits on it.
type keyedLocks struct {
	mu sync.Mutex
	m  map[string]*keyedLock
}

type keyedLock struct {
	mu   sync.Mutex
	refs int
}

// lock blocks until key is free, then holds it; the returned func releases it.
func (k *keyedLocks) lock(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = make(map[string]*keyedLock)
	}
	l := k.m[key]
	if l == nil {
		l = &keyedLock{}
		k.m[key] = l
	}
	l.refs++
	k.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		k.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
}
