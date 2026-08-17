// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

// Package handlers provides HTTP handlers for the sched API.
package handlers

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/robfig/cron/v3"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/ratelimit"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/structuredoutput"
)

// Config holds the handler configuration.
type Config struct {
	OrchestratorURL string
	AdminAPIKey     string
	Version         string
	DataDir         string
	Timezone        string
	// UploadMaxBytes caps a single POST /upload task-input file. Mirrors
	// the chat plane's cfg.UploadMaxBytes (FLEET_UPLOAD_MAX_BYTES, default
	// 1 GiB) so both upload surfaces enforce the same limit.
	UploadMaxBytes int64
	// DefaultTaskTimezone (FLEET_DEFAULT_TIMEZONE) is the IANA timezone applied
	// to a new task whose create request omits one. Distinct from Timezone
	// (FLEET_TIMEZONE, the server clock); empty defaults to "UTC".
	DefaultTaskTimezone string

	// Cost-forecast inputs (issue #233): the values the EstimateTask handler
	// needs to project a scheduled task's cost without running it. They mirror
	// the runtime selection so the forecast reflects the same model + ceilings a
	// real dispatch would use. All advisory; the forecast never gates creation.
	//
	// DefaultTaskModel is the model a task with no explicit model resolves to
	// (CUTLASS_TASK_MODEL). MaxCostUSD is the per-turn cost ceiling
	// (CUTLASS_MAX_COST_USD); 0 disables the would-hit-ceiling check.
	// DefaultMaxIterations is the loop iteration cap applied when a task omits
	// one (FLEET_MAX_ITERATIONS via config.MaxIterations).
	DefaultTaskModel     string
	MaxCostUSD           float64
	DefaultMaxIterations int

	// Sliding-window rate limits for the high-cost orchestrator endpoints
	// (POST /tasks, POST /upload), enforced by SchedRateLimitMiddleware.
	// Per-key defaults; a key's own RateLimit (when > 0) overrides the
	// per-minute cap for that key. 0 disables the corresponding window.
	// FLEET_SCHED_RATE_LIMIT_PER_MINUTE / _PER_DAY / _GLOBAL_PER_MINUTE.
	SchedRateLimitPerMinute       int // default 60
	SchedRateLimitPerDay          int // default 500
	SchedGlobalRateLimitPerMinute int // default 200; process-wide across all keys

	// SharedToken is the chat-server shared secret (CHAT_SERVER_TOKEN), reused as
	// the orchestrator's channel authenticator. The Next.js proxy — the SOLE
	// client of this backend — verifies the user's session cookie, then forwards
	// the resolved identity as X-User-Email guarded by this token in
	// X-Orchestrator-Server-Token. This is what lets a /chat-cookie user reach the
	// Operations Center without a second (moc bearer) login (#157). Reused rather
	// than a distinct secret because both backends run in ONE process and the
	// trust boundary is identical; it is impersonation-load-bearing, so the
	// orchestrator MUST stay bound to 127.0.0.1. Empty is impossible in
	// production (config.Validate makes FLEET_SERVER_TOKEN fatal-if-empty).
	SharedToken string

	// Elcano unified auth (scoped tier). ElcanoPubKey is the Ed25519 public
	// key the server verifies the elcano_auth cookie with; nil disables the
	// cookie path (the button renders but every cookie fails closed). See
	// elcano.go.
	ElcanoPubKey       ed25519.PublicKey
	AuthLoginURL       string // e.g. https://auth.elcanotek.com (no trailing slash)
	ElcanoCookieName   string // default "elcano_auth"
	ElcanoCookieDomain string // AUTH_COOKIE_DOMAIN (e.g. "elcanotek.com"); "" = host-only. Needed to delete the shared cookie on logout.

	// Per-task sandbox-limit ceilings (#205): the maxima a task's SandboxLimits
	// override may request (FLEET_SANDBOX_*_MAX). 0 = no ceiling. Mirrors the
	// config.Config values; threaded here so validateSandboxLimits can enforce them.
	SandboxMemoryMaxMB int
	SandboxCPUsMax     float64
	SandboxPidsMax     int
}

// Handlers contains all HTTP handlers.
type Handlers struct {
	config  Config
	storage *storage.Storage
	apiKeys *apikeys.Manager

	// Rate limiting for login
	loginRateLimiter *rateLimiter

	// Sliding-window limiters for the high-cost task endpoints (POST /tasks,
	// POST /upload), shared implementation with the chat server
	// (internal/ratelimit). taskKeyRL is per-API-key (or per-IP for cookie/bearer
	// callers); taskGlobalRL is a single process-wide window. See
	// ratelimit_middleware.go.
	taskKeyRL    *ratelimit.Limiter
	taskGlobalRL *ratelimit.Limiter
	// taskRLCounter counts 429s emitted by SchedRateLimitMiddleware, by window
	// ("global"|"minute"|"day"). Behind a pointer (like the caches above) so a
	// Handlers value can still be copied. Surfaced via RateLimitExceededCounts; a
	// Prometheus surface is deferred to the metrics issue (#176).
	taskRLCounter *rateLimitCounter

	// Cache for dashboard stats
	statsCache *statsCache

	// memberLookup resolves an email to a user for the scoped-tier gate
	// (elcano_auth cookie path). nil in production → falls back to
	// storage.GetUserByUsername. Tests inject a fake to avoid a live database.
	memberLookup func(ctx context.Context, email string) (*models.User, error)

	// mcpCatalog returns the read-only Optional-MCP catalog the task-form picker
	// + credential-account admin table render. Injected by cmd/fleet via
	// SetMCPCatalogProvider; nil → empty catalog. See mcp.go.
	mcpCatalog func() []MCPServerCatalogEntry

	// remoteMCPServers returns the per-user remote (hosted) MCP servers the given
	// email has connected via OAuth (#443), so GetMCPServers can surface them in
	// the task-form picker alongside the bundle catalog (#466) — mirroring chat's
	// /mcp-servers. Injected by cmd/fleet via SetRemoteMCPServersProvider from the
	// remotemcp Service; nil (feature off) → no remote entries. See mcp.go.
	remoteMCPServers func(ctx context.Context, email string) []MCPServerCatalogEntry

	// taskTemplates returns the read-only task-template catalog the task-create UI
	// renders as "new task from a template". Injected by cmd/fleet via
	// SetTaskTemplateProvider from the loaded client bundle; nil → empty catalog.
	// See task_templates.go.
	taskTemplates taskTemplateProvider

	// promptCatalog returns the bundle/Git-backed half of the hybrid prompt
	// library. Mutable private/workspace entries live in sched storage.
	promptCatalog func() ([]clientconfig.Prompt, []string)

	// taskStreamLookup resolves a task's live SSE run-log buffer (#200), wired by
	// cmd/fleet via SetTaskStreamProvider from the worker pool's registry. nil →
	// no live stream is ever available (every task falls back to the persisted log
	// one-shot replay). See task_stream.go.
	taskStreamLookup TaskStreamLookup

	// systemPromptForPersona resolves the assembled scheduled system prompt
	// (default prompt + persona expertise) for a persona override, exactly as the
	// runner assembles it before dispatch (#233 cost forecast). Wired by cmd/fleet
	// via SetSystemPromptProvider from the scheduled runner; nil → the forecast
	// counts only the task prompt + tool schemas (the system-prompt token line
	// reads 0). See estimate.go.
	systemPromptForPersona func(persona string) string

	// personaCatalog returns the persona names currently loadable from the
	// client bundle (basenames of personas/*.yaml, without the extension).
	// Wired by cmd/fleet via SetPersonaCatalog so the create paths can reject
	// an unknown persona with a clear 400 listing the valid names, instead of
	// silently dispatching on the global default (#720). nil (tests / embedders
	// without a bundle) → no existence check, only the path-safety validation.
	// The dispatch-time fallback in the scheduled runner is unchanged: a bundle
	// can drop a persona AFTER a task was created (personas hot-reload), and
	// such a task still runs on the global default rather than failing.
	personaCatalog func() []string

	// datasetRunner starts/pauses dataset runs (#514) — injected via
	// SetDatasetRunner so handlers stay decoupled from the agent graph.
	datasetRunner DatasetRunController

	// learnedDistiller turns feedback into a proposed learned instruction (#516);
	// nil = self-improvement distillation off (feedback is still recorded).
	learnedDistiller LearnedInstructionDistiller

	// taskStopper interrupts a task executing in THIS process (#508) —
	// injected from the runner pool via SetTaskStopper (mirrors
	// SetTaskStreamProvider). nil = cancel stays a DB-only transition.
	taskStopper func(taskID uuid.UUID, who string) bool

	// chatUsage aggregates the chat store's turn_metrics for the usage report
	// (#601) — injected by cmd/fleet via SetChatUsageProvider because the chat
	// store is a separate database. nil → the report covers tasks only and
	// its sources list says so. See usage.go.
	chatUsage ChatUsageProvider

	// chatUserDayUsage / chatAccounts feed the adoption report
	// (GET /admin/usage/adoption) — the chat side's per-user-per-day metering
	// and the provisioned-account roster, injected by cmd/fleet like chatUsage.
	// nil → the report covers tasks only / omits the seat denominator, and its
	// sources list says so. See adoption.go.
	chatUserDayUsage ChatUserDayUsageProvider
	chatAccounts     ChatAccountsProvider

	// chatSessionEpoch resolves an email's chat-plane session epoch so the
	// header-trust path can refuse a session cookie a password reset has already
	// evicted from chat. Injected by cmd/fleet like the seams above, because the
	// epoch lives in the chat store's users table (ADR-0005). nil → the claim is
	// not checked. See session_epoch.go.
	chatSessionEpoch ChatSessionEpochProvider

	// budgetGate enforces per-principal rolling budgets at task-create (#601
	// part 2) — injected by cmd/fleet via SetBudgetGate (*budget.Enforcer). nil
	// → no budget enforcement, today's behavior byte-for-byte. See budgets.go.
	budgetGate BudgetGate

	// agentPoolActive/agentPoolSlots report the scheduled-runner pool's live
	// occupancy for the dashboard's agent cards — injected by cmd/fleet via
	// SetAgentPoolStats. nil → GET /stats omits the agent fields.
	agentPoolActive func() int
	agentPoolSlots  func() int
}

// SetAgentPoolStats wires the live agent-pool occupancy into GET /stats:
// active = agents executing scheduled tasks now, slots = schedulable
// capacity. Call before serving traffic.
func (h *Handlers) SetAgentPoolStats(active, slots func() int) {
	h.agentPoolActive = active
	h.agentPoolSlots = slots
}

// statsCache caches dashboard statistics.
type statsCache struct {
	mu    sync.RWMutex
	store map[string]statsCacheEntry
}

type statsCacheEntry struct {
	stats     *models.DashboardStats
	expiresAt time.Time
}

func newStatsCache() *statsCache {
	return &statsCache{
		store: make(map[string]statsCacheEntry),
	}
}

func (c *statsCache) Get(key string) (*models.DashboardStats, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.store[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(entry.expiresAt) {
		return nil, false
	}
	return entry.stats, true
}

func (c *statsCache) Set(key string, stats *models.DashboardStats, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Prevent unbounded growth with a hard limit
	const maxSize = 1000
	if len(c.store) >= maxSize {
		now := time.Now()
		// First pass: try to evict expired items
		// We limit the scan to avoid locking for too long
		scanLimit := 100
		scanned := 0
		for k, v := range c.store {
			if scanned >= scanLimit {
				break
			}
			scanned++
			if now.After(v.expiresAt) {
				delete(c.store, k)
			}
		}

		// If still full, evict a random item (the current one in iteration)
		if len(c.store) >= maxSize {
			for k := range c.store {
				delete(c.store, k)
				break
			}
		}
	}

	c.store[key] = statsCacheEntry{
		stats:     stats,
		expiresAt: time.Now().Add(ttl),
	}
}

// rateLimiter provides simple per-IP rate limiting.
type rateLimiter struct {
	mu       sync.Mutex
	requests map[string][]time.Time
	limit    int
	window   time.Duration
}

const (
	taskPromptMinLength = 3
	taskPromptMaxLength = 100000
	// maxTaskDescriptionChars caps the optional operator documentation field (#281)
	// at 10k runes — generous for a runbook, bounded so it can't bloat the row.
	maxTaskDescriptionChars = 10000
	// maxTaskTitleChars caps the display label. It is rendered in a table cell
	// and a calendar tile, so it is short by design: long enough for
	// "Reklaim daily day-over-day campaign health scan", short enough that no
	// title can push a column off screen.
	maxTaskTitleChars    = 120
	taskScheduleMaxYears = 5
)

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		requests: make(map[string][]time.Time),
		limit:    limit,
		window:   window,
	}
}

func (rl *rateLimiter) Allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-rl.window)

	// Clean old requests for this IP
	times := rl.requests[ip]
	recent := times[:0]
	for _, t := range times {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	rl.requests[ip] = recent

	// Incrementally clean up stale IPs to avoid O(N) spikes
	// If the map is large, we check a few random entries on every request.
	// We use a fixed sample size to ensure O(1) performance regardless of map size.
	if len(rl.requests) > 100 {
		// Sample a fixed number of entries (e.g. 50)
		cleanupSamples := 50

		checked := 0
		for otherIP, times := range rl.requests {
			if checked >= cleanupSamples {
				break
			}
			checked++

			// Skip the current IP (we just processed it)
			if otherIP == ip {
				continue
			}

			// Delete if empty or all requests are old
			// Since times is sorted, checking the last element is sufficient
			if len(times) == 0 || times[len(times)-1].Before(cutoff) {
				delete(rl.requests, otherIP)
			}
		}
	}

	if len(recent) >= rl.limit {
		return false
	}

	rl.requests[ip] = append(rl.requests[ip], now)
	return true
}

// New creates a new Handlers instance. The sliding-window task limiters take
// their bounds verbatim from cfg (0 disables a window) — cmd/fleet applies the
// 60/min, 500/day, 200/min-global defaults when reading the env, so a zero here
// means an operator (or test) explicitly disabled it.
func New(cfg Config, store *storage.Storage, keyMgr *apikeys.Manager) *Handlers {
	return &Handlers{
		config:           cfg,
		storage:          store,
		apiKeys:          keyMgr,
		loginRateLimiter: newRateLimiter(20, time.Minute), // 20 logins per minute per IP
		taskKeyRL:        ratelimit.New(cfg.SchedRateLimitPerMinute, cfg.SchedRateLimitPerDay),
		taskGlobalRL:     ratelimit.New(cfg.SchedGlobalRateLimitPerMinute, 0),
		taskRLCounter:    newRateLimitCounter(),
		statsCache:       newStatsCache(),
	}
}

// Helper functions

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The status line + headers are already committed, so a mid-body encode
	// failure (typically the client disconnecting) can't change the response —
	// log it for diagnostics rather than swallowing it silently.
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writeJSON: failed to encode response body: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, models.ErrorResponse{Detail: detail})
}

// logSafe strips CR/LF (and stray carriage returns) from a value before it is
// interpolated into a log line, so attacker-controlled strings (e.g. a key_id
// taken straight from the URL path, or an uploaded filename) cannot forge or
// split log entries. gosec flags these as G706 (log injection via taint
// analysis); this is the real mitigation for the ones that carry untrusted
// text.
func logSafe(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}

func readJSON(r *http.Request, v interface{}) error {
	return json.NewDecoder(r.Body).Decode(v)
}

// getClientIP extracts the client IP from the request.
// Prefers the IP resolved by chi's ClientIPFromXFF middleware (see main.go),
// which walks X-Forwarded-For right-to-left past trusted proxy hops and never
// trusts client-supplied values. Falls back to the connection's RemoteAddr for
// direct (proxyless) access, e.g. local development.
func getClientIP(r *http.Request) string {
	if ip := middleware.GetClientIP(r.Context()); ip != "" {
		return ip
	}
	// Direct connection: parse RemoteAddr.
	// Format is typically "ip:port" or just "ip" for IPv6
	addr := r.RemoteAddr
	if idx := strings.LastIndex(addr, ":"); idx != -1 {
		// Check if this is IPv6 (contains multiple colons)
		if strings.Count(addr, ":") > 1 {
			// IPv6 address - if wrapped in brackets, extract it
			if strings.HasPrefix(addr, "[") {
				if end := strings.Index(addr, "]"); end != -1 {
					return addr[1:end]
				}
			}
			return addr
		}
		// IPv4 with port
		return addr[:idx]
	}
	return addr
}

// Auth middleware helpers

func (h *Handlers) verifyAdminKey(r *http.Request) bool {
	// Fail closed when no admin key is configured. Otherwise sha256("") on both
	// sides would match a request that sends NO X-API-Key header, silently
	// authenticating it as admin — every caller (both admin middlewares, the
	// principal resolver, and the inline handlers in batch/upload) would then
	// grant full access on a deployment that simply left ADMIN_API_KEY unset.
	// Guarding here closes all of them at once; the duplicate guard in
	// SchedRateLimitMiddleware is now redundant but harmless.
	if h.config.AdminAPIKey == "" {
		return false
	}
	apiKey := r.Header.Get("X-API-Key")
	// Hash inputs to prevent length-deduction timing attacks before constant-time comparison
	apiKeyHash := sha256.Sum256([]byte(apiKey))
	expectedKeyHash := sha256.Sum256([]byte(h.config.AdminAPIKey))
	return subtle.ConstantTimeCompare(apiKeyHash[:], expectedKeyHash[:]) == 1
}

// Task Management Endpoints

// CreateTask handles POST /tasks
func (h *Handlers) CreateTask(w http.ResponseWriter, r *http.Request) {
	// Authorization is the SHARED authorizeTaskCreator — the extracted form of
	// the auth block this handler used to inline (admin key, scoped key with
	// create permission, user bearer token, Elcano cookie member), also used by
	// the batch path so the two cannot drift. It resolves the creator id, the
	// authorizing key (for spend attribution), and the username the budget gate
	// keys user-scope budgets on (#601 part 2).
	creator, ok := h.authorizeTaskCreator(w, r)
	if !ok {
		return
	}

	var tc models.TaskCreate
	if err := readJSON(r, &tc); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	// Lineage is server-authoritative (#277): created_by_task_id is set ONLY by
	// the in-process create_task tool when a scheduled run spawns a follow-up. An
	// external client must not be able to forge a spawn lineage, so clear any
	// value it supplied on the public create path.
	tc.CreatedByTaskID = nil

	if err := h.validateTaskCreate(&tc); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if msg := requireAdminForRunIf(creator.hasAdminPermission, tc.RunIf); msg != "" {
		writeError(w, http.StatusForbidden, msg)
		return
	}

	// Per-principal rolling budget (#601 part 2): the SAME budgetCapError gate
	// the batch and chat schedule_task paths run. At a hard bound the create is
	// refused (402 + Retry-After at the window rollover); a soft crossing fires
	// its once-per-window alert inside the gate.
	if err := h.budgetCapError(r.Context(), creator); err != nil {
		writeBudgetRefusal(w, err)
		return
	}

	task := models.NewTask(tc)
	task.CreatedBy = creator.creatorID
	task.CreatedByKeyID = creator.creatorKey

	// Per-key priority ceiling (#230): a scoped key capped at max_priority may not
	// submit a task MORE urgent (lower integer) than that. task.Priority is the
	// post-default value (0→Normal), so the comparison reflects what would run.
	// Shares priorityCapError with the batch path so the two can't drift.
	if err := priorityCapError(creator.creatorKeyMaxPriority, task.Priority); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}

	if _, err := h.storage.AddTask(task); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to create task")
		return
	}

	log.Printf("Task created: %s (prompt: %.50s...)", task.ID, task.Prompt)
	localizeTask(task)
	writeJSON(w, http.StatusOK, task)
}

// queueTierBands groups the 0–100 priority scale into the named reporting tiers
// for GET /admin/queue (#230). Inclusive [lo,hi], ordered most→least urgent;
// each named constant (Critical=10, High=25, Normal=50, Low=75, Bulk=90) and the
// starvation floor (25) falls inside its band, and the bands tile [0,100] with
// no gaps so every pending row is counted exactly once.
var queueTierBands = []struct {
	name   string
	lo, hi int
}{
	{"critical", 0, 19},
	{"high", 20, 39},
	{"normal", 40, 59},
	{"low", 60, 79},
	{"bulk", 80, 100},
}

// QueueStats handles GET /admin/queue: the operator's view of the pending task
// queue (#230) — total depth, the oldest pending wait, and the depth + oldest
// wait per named priority tier, so backlog and starvation are visible at a
// glance. Admin-gated by the route group.
func (h *Handlers) QueueStats(w http.ResponseWriter, r *http.Request) {
	buckets, err := h.storage.PendingQueueStats(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to read queue stats")
		return
	}
	stats := models.QueueStats{Tiers: make([]models.QueueTierStat, len(queueTierBands))}
	for i, band := range queueTierBands {
		stats.Tiers[i] = models.QueueTierStat{Tier: band.name, MinPriority: band.lo, MaxPriority: band.hi}
	}
	for _, b := range buckets {
		stats.PendingTotal += b.Count
		if b.OldestAgeSeconds > stats.OldestAgeSeconds {
			stats.OldestAgeSeconds = b.OldestAgeSeconds
		}
		for i := range queueTierBands {
			if b.Priority >= queueTierBands[i].lo && b.Priority <= queueTierBands[i].hi {
				stats.Tiers[i].Count += b.Count
				if b.OldestAgeSeconds > stats.Tiers[i].OldestAgeSeconds {
					stats.Tiers[i].OldestAgeSeconds = b.OldestAgeSeconds
				}
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, stats)
}

// validateTaskLimits bounds the per-task numeric ceilings. max_retries is
// bounded because an unbounded value, combined with the 10-minute backoff cap,
// would let a deterministically-failing task re-queue forever and hold a
// scheduler slot.
func validateTaskLimits(tc *models.TaskCreate) error {
	if tc.MaxIterations != nil && (*tc.MaxIterations < 1 || *tc.MaxIterations > 10000) {
		return fmt.Errorf("max_iterations must be between 1 and 10000")
	}
	if tc.MaxRetries != nil && (*tc.MaxRetries < 0 || *tc.MaxRetries > 10) {
		return fmt.Errorf("max_retries must be between 0 and 10")
	}
	// Per-task thinking override (#220): nil = inherit; 0 = off; >0 = budget
	// (clamped to the provider bounds at run time). A negative value is nonsense.
	if tc.ThinkingBudgetTokens != nil && *tc.ThinkingBudgetTokens < 0 {
		return fmt.Errorf("thinking_budget_tokens must be >= 0 (0 = off, omit to inherit the default)")
	}
	// Priority is bounded to [0,100] (#230); lower = more urgent. 0 is the unset
	// sentinel that NewTask maps to Normal (50), so it is accepted here.
	if tc.Priority < models.PriorityMin || tc.Priority > models.PriorityMax {
		return fmt.Errorf("priority must be between %d and %d", models.PriorityMin, models.PriorityMax)
	}
	// SLA config (#274): a statically-broken triple (non-positive expected
	// duration, or a fail threshold at or below the warn) would fire spurious
	// alerts on every run — reject it while the author is still looking at the
	// request. Runs through every create path (create, edit, import, estimate)
	// because they all funnel into validateTaskCreate.
	if err := models.ValidateSLA(tc.ExpectedDurationMinutes, tc.SLAWarnMultiplier, tc.SLAFailMultiplier); err != nil {
		return err
	}
	return nil
}

// minSandboxMemoryMB / minSandboxPids are the per-task floors below which a
// container is too small to be useful; the operator ceilings come from config (#205).
const (
	minSandboxMemoryMB = 128
	minSandboxPids     = 16
)

// validateSandboxLimits bounds an optional per-task sandbox override (#205): each
// set field must clear a sane floor and must not exceed the operator-configured
// ceiling (FLEET_SANDBOX_*_MAX). A zero field means "use the global default" and
// is left alone. nil = no override.
func (h *Handlers) validateSandboxLimits(l *models.TaskSandboxLimits) error {
	if l == nil {
		return nil
	}
	if l.MemoryMB != 0 {
		if l.MemoryMB < minSandboxMemoryMB {
			return fmt.Errorf("sandbox_limits.memory_mb must be >= %d", minSandboxMemoryMB)
		}
		if ceiling := h.config.SandboxMemoryMaxMB; ceiling > 0 && l.MemoryMB > ceiling {
			return fmt.Errorf("sandbox_limits.memory_mb %d exceeds operator ceiling %d", l.MemoryMB, ceiling)
		}
	}
	if l.CPUs != 0 {
		if l.CPUs < 0 {
			return fmt.Errorf("sandbox_limits.cpus must be > 0")
		}
		if ceiling := h.config.SandboxCPUsMax; ceiling > 0 && l.CPUs > ceiling {
			return fmt.Errorf("sandbox_limits.cpus %.2f exceeds operator ceiling %.2f", l.CPUs, ceiling)
		}
	}
	if l.Pids != 0 {
		if l.Pids < minSandboxPids {
			return fmt.Errorf("sandbox_limits.pids must be >= %d", minSandboxPids)
		}
		if ceiling := h.config.SandboxPidsMax; ceiling > 0 && l.Pids > ceiling {
			return fmt.Errorf("sandbox_limits.pids %d exceeds operator ceiling %d", l.Pids, ceiling)
		}
	}
	return nil
}

// defaultTaskTimezone is the IANA timezone applied to a task created without an
// explicit one. FLEET_DEFAULT_TIMEZONE, then "UTC".
func (h *Handlers) defaultTaskTimezone() string {
	if tz := strings.TrimSpace(h.config.DefaultTaskTimezone); tz != "" {
		return tz
	}
	return "UTC"
}

// resolveTaskTimezone resolves and validates tc.Timezone — defaulting an empty
// value to the server default — and writes the resolved name back to tc so it
// persists with the task. Returns the loaded location for cron evaluation, or an
// error when the name is not a valid IANA timezone.
func (h *Handlers) resolveTaskTimezone(tc *models.TaskCreate) (*time.Location, error) {
	tzName := strings.TrimSpace(tc.Timezone)
	if tzName == "" {
		tzName = h.defaultTaskTimezone()
	}
	loc, err := time.LoadLocation(tzName)
	if err != nil {
		return nil, fmt.Errorf("unknown timezone %q: must be a valid IANA timezone name (e.g. America/New_York)", tzName)
	}
	tc.Timezone = tzName
	return loc, nil
}

// localizeTask populates NextRunAtLocal from ScheduledFor rendered in the task's
// timezone (RFC3339 with offset), so callers can display local time without any
// client-side timezone math. No-op when the task has no scheduled_for.
func localizeTask(task *models.Task) {
	if task == nil || task.ScheduledFor == nil {
		return
	}
	loc, err := time.LoadLocation(task.Timezone)
	if err != nil {
		loc = time.UTC
	}
	local := task.ScheduledFor.In(loc).Format(time.RFC3339)
	task.NextRunAtLocal = &local
}

// localizeTasks applies localizeTask across a slice (the list endpoints).
func localizeTasks(tasks []*models.Task) {
	for _, t := range tasks {
		localizeTask(t)
	}
}

// validateTaskRouting validates the task's targeting/routing fields: the trigger
// type (#177) and the per-task MCP selection. Split out of validateTaskCreate to
// keep that function under the gocyclo threshold.
func (h *Handlers) validateTaskRouting(tc *models.TaskCreate) error {
	// Reject an unrecognized trigger type up front (#177). Empty defaults to
	// "cron" in NewTask, so only a non-empty invalid value is an error.
	if tc.TriggerType != "" && !tc.TriggerType.IsValid() {
		return fmt.Errorf("trigger_type %q is not valid (want cron or webhook)", tc.TriggerType)
	}

	// Light validation of the per-task MCP selection: each chosen server must
	// be named. Account is optional (empty means the default/shared seat).
	for _, choice := range tc.MCPSelection {
		if strings.TrimSpace(choice.Server) == "" {
			return fmt.Errorf("mcp_selection entries must name a server")
		}
	}
	// Same for the credential allowlist (#184): a nil allowlist inherits global,
	// but every explicit entry must name a server.
	for _, entry := range tc.CredentialAllowlist {
		if strings.TrimSpace(entry.Server) == "" {
			return fmt.Errorf("credential_allowlist entries must name a server")
		}
	}
	// Loop config (#179): a nil config is an ordinary one-shot task; a non-nil
	// config must name a recognized, compilable exit condition — fail fast at
	// creation rather than always-exhaust at runtime.
	if tc.LoopConfig != nil {
		if err := tc.LoopConfig.ValidateExitCondition(); err != nil {
			return fmt.Errorf("loop_config: %w", err)
		}
	}
	// Worktree config (#180): a nil config shares the workspace; a non-nil
	// enabled config must be internally consistent (valid branch prefix,
	// non-negative cleanup delay) — fail fast at creation.
	if err := tc.WorktreeConfig.Validate(); err != nil {
		return fmt.Errorf("worktree_config: %w", err)
	}
	// Title: the optional display label. Normalized to a single trimmed line —
	// it is rendered inline in a table cell and a calendar tile, where an
	// embedded newline is at best ignored and at worst breaks the row, and a
	// caller pasting a multi-line prompt into the title field should be told so
	// rather than silently getting a mangled label.
	tc.Title = strings.TrimSpace(tc.Title)
	if strings.ContainsAny(tc.Title, "\r\n") {
		return fmt.Errorf("title must be a single line")
	}
	if utf8.RuneCountInString(tc.Title) > maxTaskTitleChars {
		return fmt.Errorf("title cannot exceed %d characters", maxTaskTitleChars)
	}
	// Description (#281): optional operator documentation, bounded so it can't
	// bloat the task row. Counted in runes (not bytes) to be Unicode-fair.
	if utf8.RuneCountInString(tc.Description) > maxTaskDescriptionChars {
		return fmt.Errorf("description exceeds maximum length of %d characters", maxTaskDescriptionChars)
	}
	// Tags (#212): normalize in place to the canonical (lowercased, deduped,
	// validated) form so the persisted value is consistent and filterable.
	normalizedTags, err := models.NormalizeAndValidateTags(tc.Tags)
	if err != nil {
		return fmt.Errorf("tags: %w", err)
	}
	tc.Tags = normalizedTags
	// Retry policy (#201): nil → legacy backoff; non-nil must be internally
	// consistent (valid backoff type, non-negative ordered delays, known classes).
	if err := tc.RetryPolicy.Validate(); err != nil {
		return fmt.Errorf("retry_policy: %w", err)
	}
	// Persona (#221): a per-task persona override must be a single bundle filename
	// component (no path separators / traversal) so it resolves to
	// personas/<name>.yaml.
	if persona := strings.TrimSpace(tc.Persona); persona != "" {
		if strings.ContainsAny(persona, `/\`) || strings.Contains(persona, "..") || filepath.Base(persona) != persona {
			return fmt.Errorf("persona must be a bundle persona name without a path (got %q)", persona)
		}
		// Existence check (#720): when the persona catalog is wired, an unknown
		// name is a fail-fast 400 listing the valid names — a persona typo used
		// to silently dispatch on the global default, producing wrong-persona
		// runs with no error. The dispatch-time fallback stays for tasks whose
		// persona disappears from the bundle AFTER creation (bundles hot-reload).
		if h.personaCatalog != nil {
			names := h.personaCatalog()
			if !slices.Contains(names, persona) {
				if len(names) == 0 {
					return fmt.Errorf("persona %q is not in the loaded client bundle (the bundle declares no personas)", persona)
				}
				return fmt.Errorf("persona %q is not in the loaded client bundle (valid personas: %s)",
					persona, strings.Join(names, ", "))
			}
		}
	}
	// RunIf pre-run gate (#269): nil = the legacy unconditional promotion path.
	// A non-nil gate must have a non-empty command, a valid on_error policy, and
	// a timeout in [1, 300]s — fail fast at creation rather than always-skip
	// or always-run at runtime.
	if err := tc.RunIf.Validate(); err != nil {
		return fmt.Errorf("run_if: %w", err)
	}
	return nil
}

// requireAdminForRunIf enforces the run_if privilege boundary. A run_if gate
// executes ON THE HOST as the fleet user (scheduler.go documents it as trusted
// "exactly like the fleet binary itself") — structural validation cannot make
// an arbitrary creator's shell string safe, so only a principal carrying
// models.PermissionAdmin (taskCreator.hasAdminPermission on the create paths,
// principal.hasPermission(models.PermissionAdmin) on the edit path) may attach
// or change one. Returns the 403 message, or "" when allowed.
func requireAdminForRunIf(hasAdminPermission bool, runIf *models.RunIf) string {
	if runIf != nil && !hasAdminPermission {
		return "run_if: a host-side pre-run gate can only be set by an admin"
	}
	return ""
}

func (h *Handlers) validateTaskCreate(tc *models.TaskCreate) error { //nolint:gocyclo // centralized fail-fast validation keeps every create path identical.
	tc.Prompt = strings.TrimSpace(tc.Prompt)
	if tc.Prompt == "" {
		return fmt.Errorf("prompt is required")
	}
	if len(tc.Prompt) < taskPromptMinLength {
		return fmt.Errorf("prompt must be at least %d characters", taskPromptMinLength)
	}
	if len(tc.Prompt) > taskPromptMaxLength {
		return fmt.Errorf("prompt cannot exceed %d characters", taskPromptMaxLength)
	}

	if err := h.validateTaskRouting(tc); err != nil {
		return err
	}

	if err := normalizeOptionalModel(&tc.Model, "model"); err != nil {
		return err
	}
	if err := normalizeOptionalModel(&tc.FallbackModel, "fallback_model"); err != nil {
		return err
	}
	// A task with neither its own model nor a deployment default can never run:
	// the dispatcher fails it terminally on the first attempt and dead-letters it
	// (#1014). Refuse it here instead, so the author sees the problem while they
	// are still looking at the request rather than up to a cron period later, in
	// the DLQ, having published nothing.
	if tc.Model == nil && strings.TrimSpace(h.config.DefaultTaskModel) == "" {
		return fmt.Errorf("no model configured: set FLEET_TASK_MODEL on the orchestrator, or pass model on the task")
	}
	if err := validateTaskLimits(tc); err != nil {
		return err
	}
	if err := h.validateSandboxLimits(tc.SandboxLimits); err != nil {
		return err
	}
	// Structured-output mode (#244): reject a malformed output_schema up front so
	// the task author sees the error at create time, not at run time.
	if len(tc.OutputSchema) > 0 {
		if err := structuredoutput.ValidateSchema(tc.OutputSchema); err != nil {
			return fmt.Errorf("output_schema: %w", err)
		}
	}

	// Resolve + validate the per-task timezone (writes the resolved name back to
	// tc so it persists). The cron Recurrence is evaluated in this zone so a
	// "9am" task fires at 9am local, not 9am UTC.
	loc, err := h.resolveTaskTimezone(tc)
	if err != nil {
		return err
	}

	if tc.Recurrence != "" {
		tc.Recurrence = strings.TrimSpace(tc.Recurrence)
		if tc.Recurrence == "" {
			tc.Recurrence = ""
		} else {
			schedule, err := cron.ParseStandard(tc.Recurrence)
			if err != nil {
				return fmt.Errorf("recurrence must be a standard 5-field cron expression")
			}
			// If no explicit scheduled_for was provided, set it to the next
			// cron trigger time so the task waits instead of running immediately.
			// scheduled_for is always stored as an absolute UTC instant.
			if tc.ScheduledFor == nil {
				next := schedule.Next(time.Now().In(loc)).UTC()
				tc.ScheduledFor = &next
			}
		}
	}

	// Recurrence end conditions are only meaningful on a recurring task, and a
	// run budget of zero would be a task that never runs — reject both shapes
	// at the boundary. A recurrence_until already in the past is allowed (the
	// first occurrence still runs; the chain just doesn't continue) so exported
	// definitions stay importable.
	if tc.RecurrenceUntil != nil && tc.Recurrence == "" {
		return fmt.Errorf("recurrence_until requires a recurrence")
	}
	if tc.RecurrenceRemaining != nil {
		if tc.Recurrence == "" {
			return fmt.Errorf("recurrence_remaining requires a recurrence")
		}
		if *tc.RecurrenceRemaining < 1 || *tc.RecurrenceRemaining > 10000 {
			return fmt.Errorf("recurrence_remaining must be between 1 and 10000")
		}
	}

	if tc.ScheduledFor != nil {
		scheduled := tc.ScheduledFor.UTC()
		now := time.Now().UTC()
		if scheduled.Before(now) {
			return fmt.Errorf("scheduled time cannot be in the past")
		}
		maxScheduled := now.AddDate(taskScheduleMaxYears, 0, 0)
		if scheduled.After(maxScheduled) {
			return fmt.Errorf("scheduled time is too far in the future")
		}
		tc.ScheduledFor = &scheduled
	}

	if len(tc.Files) > 0 {
		if len(tc.FileNames) > 0 && len(tc.FileNames) != len(tc.Files) {
			return fmt.Errorf("file_names must pair 1:1 with files")
		}
		logicalNames := make(map[string]struct{}, len(tc.FileNames))
		for _, name := range tc.FileNames {
			trimmed := strings.TrimSpace(name)
			if trimmed == "" || trimmed != filepath.Base(trimmed) || !filepath.IsLocal(trimmed) || strings.ContainsAny(trimmed, `/\\`) {
				return fmt.Errorf("invalid logical file name")
			}
			if _, exists := logicalNames[trimmed]; exists {
				return fmt.Errorf("duplicate logical file name: %s", trimmed)
			}
			logicalNames[trimmed] = struct{}{}
		}
		// Deduplicate filenames to avoid redundant I/O
		uniqueFiles := make(map[string]struct{})
		for _, file := range tc.Files {
			trimmed := strings.TrimSpace(file)
			if trimmed == "" {
				return fmt.Errorf("file names cannot be empty")
			}
			if strings.Contains(trimmed, "..") || strings.Contains(trimmed, "/") || strings.Contains(trimmed, "\\") {
				return fmt.Errorf("invalid file name")
			}
			uniqueFiles[trimmed] = struct{}{}
		}

		// Convert map to slice for easier chunking
		fileSlice := make([]string, 0, len(uniqueFiles))
		for fname := range uniqueFiles {
			fileSlice = append(fileSlice, fname)
		}

		// Check existence concurrently to avoid blocking on slow disks
		var wg sync.WaitGroup
		errChan := make(chan error, 1) // Only need the first error

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Determine number of workers (limit concurrency to avoid FD pressure)
		numWorkers := 20
		if len(fileSlice) < numWorkers {
			numWorkers = len(fileSlice)
		}

		// Channel to distribute work
		workChan := make(chan string, len(fileSlice))
		for _, fname := range fileSlice {
			workChan <- fname
		}
		close(workChan)

		for i := 0; i < numWorkers; i++ {
			wg.Add(1)
			go func() {
				defer safe.Recover("sched.handlers.checksum_worker", nil)
				defer wg.Done()
				for name := range workChan {
					select {
					case <-ctx.Done():
						return
					default:
					}

					path := filepath.Join(h.config.DataDir, "temp_uploads", name)
					if _, err := os.Stat(path); err != nil {
						select {
						case errChan <- fmt.Errorf("file not found: %s", name):
							cancel()
						default:
						}
					}
				}
			}()
		}

		wg.Wait()
		close(errChan)

		if err := <-errChan; err != nil {
			return err
		}
	}
	if len(tc.Files) == 0 && len(tc.FileNames) > 0 {
		return fmt.Errorf("file_names requires files")
	}

	return nil
}

func normalizeOptionalModel(value **string, fieldName string) error {
	if value == nil || *value == nil {
		return nil
	}

	trimmed := strings.TrimSpace(**value)
	if trimmed == "" {
		*value = nil
		return nil
	}
	if len(trimmed) > 200 {
		return fmt.Errorf("%s cannot exceed 200 characters", fieldName)
	}
	if strings.ContainsAny(trimmed, "\r\n") {
		return fmt.Errorf("%s must be a single line", fieldName)
	}
	*value = &trimmed
	return nil
}

// ListTasks handles GET /tasks
// Requires pagination with ?limit=N&offset=M query parameters.
// Optional filter parameters:
//   - status: Filter by task status (pending, running, success, error, cancelled)
//   - q: Search in prompt or task ID (case-insensitive substring match)
//   - scheduled_only: If "true", only return tasks with scheduled_for or recurrence
//   - completed_today: If "true", only return tasks completed today
//   - completed_status: When completed_today=true, filter by this status (success/error)
//
// Returns a PaginatedResponse with total count.
func (h *Handlers) ListTasks(w http.ResponseWriter, r *http.Request) {
	p := h.principalFromRequest(r)
	if !p.hasPermission(models.PermissionViewTasks) {
		writeError(w, http.StatusForbidden, "Insufficient permissions")
		return
	}
	user := p.user

	// Parse pagination parameters (default: limit=50, offset=0)
	limit := 50
	offset := 0

	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		parsed, err := strconv.Atoi(limitStr)
		if err != nil || parsed < 1 || parsed > 500 {
			writeError(w, http.StatusBadRequest, "Invalid limit parameter (must be 1-500)")
			return
		}
		limit = parsed
	}

	if offsetStr := r.URL.Query().Get("offset"); offsetStr != "" {
		parsed, err := strconv.Atoi(offsetStr)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, "Invalid offset parameter")
			return
		}
		offset = parsed
	}

	// Parse filter parameters
	var filter db.TaskFilter
	hasFilters := false

	// Own-rows visibility (#1082): a principal without the fleet-wide grant
	// (admin / view_all_logs) lists only tasks it created — filtered in SQL so
	// pagination totals stay correct. Layered UNDER the caller's filters: a
	// created_by=<someone else> query from a scoped principal ANDs to nothing
	// rather than widening.
	if !p.fleetWideTaskVisibility() {
		switch {
		case p.user != nil:
			filter.VisibleToUserID = &p.user.ID
		case p.apiKey != nil:
			filter.VisibleToKeyID = &p.apiKey.KeyID
		}
		hasFilters = true
	}

	if status := r.URL.Query().Get("status"); status != "" {
		filter.Status = &status
		hasFilters = true
	}

	if q := r.URL.Query().Get("q"); q != "" {
		filter.Query = &q
		hasFilters = true
	}

	if scheduledOnly := r.URL.Query().Get("scheduled_only"); scheduledOnly == "true" {
		filter.ScheduledOnly = true
		hasFilters = true
	}

	if hasDescription := r.URL.Query().Get("has_description"); hasDescription == "true" {
		filter.HasDescription = true
		hasFilters = true
	}

	// Tag filter (#212): ?tag=a&tag=b → tasks carrying ALL of a,b. Query tags are
	// lowercased/trimmed to match the stored canonical form; blanks are dropped.
	if rawTags := r.URL.Query()["tag"]; len(rawTags) > 0 {
		for _, t := range rawTags {
			if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
				filter.Tags = append(filter.Tags, t)
			}
		}
		if len(filter.Tags) > 0 {
			hasFilters = true
		}
	}

	// Lineage filter (#270): ?source_task_id=<uuid> → re-runs/clones of that task.
	if src := strings.TrimSpace(r.URL.Query().Get("source_task_id")); src != "" {
		sid, perr := uuid.Parse(src)
		if perr != nil {
			writeError(w, http.StatusBadRequest, "Invalid source_task_id parameter")
			return
		}
		filter.SourceTaskID = &sid
		hasFilters = true
	}

	if completedToday := r.URL.Query().Get("completed_today"); completedToday == "true" {
		filter.CompletedToday = true
		hasFilters = true

		if completedStatus := r.URL.Query().Get("completed_status"); completedStatus != "" {
			filter.CompletedStatus = &completedStatus
		}
	}

	// Parse created_by filter - supports UUID or "me" for current user
	if createdByStr := r.URL.Query().Get("created_by"); createdByStr != "" {
		if createdByStr == "me" {
			// Use current user's ID - requires authentication
			if user == nil {
				writeError(w, http.StatusUnauthorized, "created_by=me requires authentication")
				return
			}
			filter.CreatedBy = &user.ID
			hasFilters = true
		} else {
			// Try to parse as UUID
			createdByID, err := uuid.Parse(createdByStr)
			if err != nil {
				writeError(w, http.StatusBadRequest, "Invalid created_by parameter: must be 'me' or a valid UUID")
				return
			}
			filter.CreatedBy = &createdByID
			hasFilters = true
		}
	}

	var tasks []*models.Task
	var total int
	var err error

	if hasFilters {
		tasks, total, err = h.storage.GetTasksFiltered(filter, limit, offset)
	} else {
		tasks, total, err = h.storage.GetAllTasksPaginated(limit, offset)
	}

	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list tasks")
		return
	}

	// Populate CreatedByUsername for display
	if err := h.populateCreatedByUsernames(r.Context(), tasks); err != nil {
		log.Printf("Warning: failed to populate creator usernames: %v", err)
		// Continue without usernames - will fall back to UUID display on frontend
	}
	localizeTasks(tasks)

	writeJSON(w, http.StatusOK, models.PaginatedResponse{
		Data:   tasks,
		Total:  total,
		Limit:  limit,
		Offset: offset,
	})
}

// GetTask handles GET /tasks/{task_id}
func (h *Handlers) GetTask(w http.ResponseWriter, r *http.Request) {
	p := h.principalFromRequest(r)
	if !p.hasPermission(models.PermissionViewTasks) {
		writeError(w, http.StatusForbidden, "Insufficient permissions")
		return
	}

	taskIDStr := chi.URLParam(r, "task_id")
	taskID, err := uuid.Parse(taskIDStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid task ID")
		return
	}

	task, err := h.storage.GetTask(taskID)
	if err != nil || task == nil {
		writeError(w, http.StatusNotFound, "Task not found")
		return
	}

	// Own-rows visibility (#1082). 403 after load, mirroring logReadableTask:
	// task ids are unguessable UUIDs, and the list endpoint no longer exposes
	// other principals' ids to scope an oracle against.
	if !taskVisibleToPrincipal(p, task) {
		writeError(w, http.StatusForbidden, "Tasks are private to their creator")
		return
	}

	// Populate CreatedByUsername for display
	if err := h.populateCreatedByUsernames(r.Context(), []*models.Task{task}); err != nil {
		log.Printf("Warning: failed to populate creator username: %v", err)
	}
	localizeTask(task)

	// For a looped task (#179), embed its per-iteration telemetry so a caller can
	// see how many cycles ran and what each verification returned. Only queried
	// for looped tasks — an ordinary one-shot task returns the bare Task as before.
	if task.LoopConfig != nil {
		iterations, ierr := h.storage.ListTaskIterations(r.Context(), taskID)
		if ierr != nil {
			log.Printf("Warning: failed to load task iterations: %v", ierr)
		}
		writeJSON(w, http.StatusOK, struct {
			*models.Task
			Iterations []*models.TaskIteration `json:"iterations"`
		}{Task: task, Iterations: iterations})
		return
	}

	writeJSON(w, http.StatusOK, task)
}

// GetTaskOutput handles GET /tasks/{task_id}/output (#244): the validated
// structured JSON result of a structured-output task, returned raw with
// Content-Type application/json (no envelope) for direct programmatic
// consumption. A successful task that declared output_schema always has this
// value. 404 means the task did not declare a schema; 409 means the value is not
// ready, the task failed its contract, or a legacy row violates the invariant.
func (h *Handlers) GetTaskOutput(w http.ResponseWriter, r *http.Request) {
	p := h.principalFromRequest(r)
	if !p.hasPermission(models.PermissionViewTasks) {
		writeError(w, http.StatusForbidden, "Insufficient permissions")
		return
	}

	taskIDStr := chi.URLParam(r, "task_id")
	taskID, err := uuid.Parse(taskIDStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid task ID")
		return
	}

	task, err := h.storage.GetTask(taskID)
	if err != nil || task == nil {
		writeError(w, http.StatusNotFound, "Task not found")
		return
	}

	// Own-rows visibility (#1082): the structured result is at least as
	// sensitive as the task row itself.
	if !taskVisibleToPrincipal(p, task) {
		writeError(w, http.StatusForbidden, "Tasks are private to their creator")
		return
	}

	if len(task.OutputSchema) == 0 {
		writeError(w, http.StatusNotFound, "Task did not declare output_schema")
		return
	}

	// Never expose a stale candidate from a failed/replayed/active row. A result
	// becomes public only with the terminal success that committed it. The
	// status codes are deliberately distinct so a polling client can tell
	// "retry later" (409, non-terminal) from "will never exist" (410, terminal
	// failure) — collapsing both onto 409 made old-contract clients poll a
	// dead task forever.
	if task.Status != models.TaskStatusSuccess {
		if !task.Status.IsTerminal() {
			writeError(w, http.StatusConflict, "Task has not finished; structured output is not yet available")
			return
		}
		writeError(w, http.StatusGone, "Task failed terminally; the declared structured output will never be available for this run")
		return
	}
	if len(task.OutputJSON) == 0 {
		// Defensive handling for legacy rows created before the fail-closed
		// storage invariant: this is a contract violation, not a missing optional
		// resource, and must never masquerade as a 404 or a retriable 409.
		writeError(w, http.StatusInternalServerError, "Structured output contract violated: successful task has no output_json")
		return
	}
	// Serve the bytes the atomic success commit validated. Deliberately NOT
	// re-validated here: re-running the validator on read would apply today's
	// schema complexity bounds retroactively to rows committed under earlier
	// bounds, making legacy successful output unreadable. Well-formedness is
	// still asserted — a corrupt row is a storage fault, not a client error.
	if !json.Valid(task.OutputJSON) {
		writeError(w, http.StatusInternalServerError, "Structured output contract violated: stored output_json is not valid JSON")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	// output_json was locally schema-validated before the atomic success commit
	// and was revalidated above before serving. It is application/json — not HTML
	// — so there is no XSS sink here.
	_, _ = w.Write(task.OutputJSON) //nolint:gosec // G705: validated JSON served as application/json, not an HTML/XSS context
}

// GetTaskErrorAnalysis handles GET /tasks/{task_id}/error-analysis (#317): the
// async post-failure LLM diagnosis (category + summary + remediation) for a
// terminally-failed task, returned raw as application/json. 404 when the task has
// no analysis (it didn't fail terminally, analysis was disabled, or the diagnosis
// failed / hasn't completed); 409 while the task is still non-terminal. Mirrors
// GetTaskOutput.
func (h *Handlers) GetTaskErrorAnalysis(w http.ResponseWriter, r *http.Request) {
	p := h.principalFromRequest(r)
	if !p.hasPermission(models.PermissionViewTasks) {
		writeError(w, http.StatusForbidden, "Insufficient permissions")
		return
	}

	taskIDStr := chi.URLParam(r, "task_id")
	taskID, err := uuid.Parse(taskIDStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid task ID")
		return
	}

	task, err := h.storage.GetTask(taskID)
	if err != nil || task == nil {
		writeError(w, http.StatusNotFound, "Task not found")
		return
	}

	// Own-rows visibility (#1082): the diagnosis quotes the failed run.
	if !taskVisibleToPrincipal(p, task) {
		writeError(w, http.StatusForbidden, "Tasks are private to their creator")
		return
	}

	if len(task.ErrorAnalysis) == 0 {
		// Distinguish "not ready yet" (still running — analysis only runs on a
		// terminal failure, and then asynchronously) from "never will be" (terminal,
		// but no analysis was produced) so a poller knows whether to retry.
		if !task.Status.IsTerminal() {
			writeError(w, http.StatusConflict, "Task has not finished; error analysis is not yet available")
			return
		}
		writeError(w, http.StatusNotFound, "Task has no error analysis (it did not fail terminally, analysis is disabled, or the diagnosis was not produced)")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	// error_analysis is schema-validated JSON (validated against errorAnalysisSchema
	// before persistence) served as application/json — not HTML — so no XSS sink.
	_, _ = w.Write(task.ErrorAnalysis) //nolint:gosec // G705: validated JSON served as application/json, not an HTML/XSS context
}

// GetTaskArtifacts handles GET /tasks/{task_id}/artifacts (#204): the curated
// manifest of named output files the run's agent published via publish_artifact,
// returned raw as application/json — a JSON array of {name, path, description,
// size}. Each entry's path is downloadable via the task's workspace file
// endpoint. 404 when the run published no artifacts; 409 while the task has not
// reached a terminal state (the manifest isn't final — poll again). Mirrors
// GetTaskOutput.
func (h *Handlers) GetTaskArtifacts(w http.ResponseWriter, r *http.Request) {
	p := h.principalFromRequest(r)
	if !p.hasPermission(models.PermissionViewTasks) {
		writeError(w, http.StatusForbidden, "Insufficient permissions")
		return
	}

	taskIDStr := chi.URLParam(r, "task_id")
	taskID, err := uuid.Parse(taskIDStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid task ID")
		return
	}

	task, err := h.storage.GetTask(taskID)
	if err != nil || task == nil {
		writeError(w, http.StatusNotFound, "Task not found")
		return
	}

	// The manifest is the directory index of the creator-private per-run workspace
	// (#287) — paths plus agent-authored descriptions — so it is gated with the
	// SAME creator-only ownership check as the workspace file endpoints. It stays
	// STRICTER than the taskVisibleToPrincipal check the /output and
	// /error-analysis siblings use (#1082): the view_all_logs auditor grant opens
	// transcripts and task rows, not workspace bytes, and the manifest must not
	// leak file metadata under a looser gate than the bytes it indexes (which
	// taskWorkspaceOwned already restricts to admin/creator).
	if !taskWorkspaceOwned(p, task) {
		writeError(w, http.StatusForbidden, "Artifacts are private to the task creator")
		return
	}

	if len(task.Artifacts) == 0 {
		// Distinguish "not ready yet" (still pending/running — the manifest is
		// persisted on the success path) from "never will be" (terminal, but the
		// run published nothing) so a poller knows whether to retry.
		if !task.Status.IsTerminal() {
			writeError(w, http.StatusConflict, "Task has not finished; the artifact manifest is not yet final")
			return
		}
		writeError(w, http.StatusNotFound, "Task published no artifacts")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	// artifacts is a marshaled []models.TaskArtifact served as application/json —
	// not HTML — so there is no XSS sink here.
	_, _ = w.Write(task.Artifacts) //nolint:gosec // G705: server-built JSON served as application/json, not an HTML/XSS context
}

// CleanupHistory handles POST /tasks/cleanup
func (h *Handlers) CleanupHistory(w http.ResponseWriter, r *http.Request) {
	days := 7
	if d := r.URL.Query().Get("days"); d != "" {
		parsed, err := strconv.Atoi(d)
		if err != nil || parsed < 0 {
			// A negative value pushes the retention cutoff into the future,
			// which would delete tasks regardless of age. days=0 is allowed and
			// means "purge everything already completed".
			writeError(w, http.StatusBadRequest, "Invalid days parameter (must be a non-negative integer)")
			return
		}
		days = parsed
	}

	deleted, err := h.storage.CleanupHistory(days)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to cleanup history")
		return
	}

	log.Printf("Cleaned up %d tasks older than %d days", deleted, days)
	writeJSON(w, http.StatusOK, models.CleanupResponse{DeletedCount: deleted})
}

// BulkSetTaskModel handles POST /tasks/model: re-assign the pinned model (and
// optional fallback) across SCHEDULED tasks, optionally limited to those pinned
// to from_model. dry_run returns the matched tasks WITHOUT writing. This is a
// fleet-wide mutation, so it is admin-gated (registered behind AdminAuthMiddleware,
// like CleanupHistory) — never a per-tenant write.
func (h *Handlers) BulkSetTaskModel(w http.ResponseWriter, r *http.Request) {
	var req models.BulkModelUpdate
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Model = strings.TrimSpace(req.Model)
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	if len(req.Model) > 200 || strings.ContainsAny(req.Model, "\r\n") {
		writeError(w, http.StatusBadRequest, "model must be a single line ≤200 chars")
		return
	}
	// Validate the optional fallback with the same rule the per-task path uses.
	// Distinguishing omit (keep existing) from "" (explicit clear) is the #1120
	// contract — normalizeOptionalModel nils empty strings, so we handle
	// explicit-clear ourselves.
	var fallback *string
	if req.FallbackModel != nil {
		trimmed := strings.TrimSpace(*req.FallbackModel)
		if trimmed == "" {
			empty := ""
			fallback = &empty
		} else {
			fp := &trimmed
			if err := normalizeOptionalModel(&fp, "fallback_model"); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			fallback = fp
		}
	}

	ctx := r.Context()
	if req.DryRun {
		tasks, err := h.storage.ListScheduledTasks(ctx)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to list scheduled tasks")
			return
		}
		matched := tasks
		if req.FromModel != "" {
			matched = matched[:0:0] // fresh slice, don't alias
			for _, t := range tasks {
				if t.Model != nil && *t.Model == req.FromModel { // nil-guard; mirrors WHERE model=$ (NULL excluded)
					matched = append(matched, t)
				}
			}
		}
		writeJSON(w, http.StatusOK, models.BulkModelUpdateResult{DryRun: true, MatchedCount: len(matched), Tasks: matched})
		return
	}

	updated, err := h.storage.BulkUpdateScheduledTaskModel(ctx, req.Model, fallback, req.FromModel)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to re-assign task model")
		return
	}
	fbLog := "<unchanged>"
	if fallback != nil {
		if *fallback == "" {
			fbLog = "<cleared>"
		} else {
			fbLog = *fallback
		}
	}
	log.Printf("Bulk re-assigned model=%q fallback=%s from=%q on %d scheduled task(s)", req.Model, fbLog, req.FromModel, updated)
	writeJSON(w, http.StatusOK, models.BulkModelUpdateResult{DryRun: false, UpdatedCount: updated})
}

// CancelTask handles DELETE /tasks/{task_id}
func (h *Handlers) CancelTask(w http.ResponseWriter, r *http.Request) {
	p := h.principalFromRequest(r)
	taskIDStr := chi.URLParam(r, "task_id")
	taskID, err := uuid.Parse(taskIDStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid task ID")
		return
	}

	// Admins and explicitly-scoped API keys retain the broad cancel permission.
	// A normal member may stop only a task they personally created. This makes
	// the live viewer's Stop control useful to job authors without granting them
	// the ability to interrupt a teammate's run.
	var taskForAuth *models.Task
	if !p.hasPermission(models.PermissionCancelTask) {
		taskForAuth, err = h.storage.GetTask(taskID)
		if err != nil || taskForAuth == nil {
			writeError(w, http.StatusNotFound, "Task not found")
			return
		}
		if !p.ownsTask(taskForAuth) {
			writeError(w, http.StatusForbidden, "Only the task creator or an admin can stop this run")
			return
		}
	}

	// Atomic cancel with WHO-stopped-it attribution (#508): the reason lands on
	// the task's terminal record, so an operator stop is auditable.
	who := p.stopLabel()
	task, err := h.storage.CancelTaskAtomic(taskID, "stopped by "+who)
	if err != nil {
		if strings.Contains(err.Error(), "cannot cancel") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if strings.Contains(err.Error(), "no rows") {
			writeError(w, http.StatusNotFound, "Task not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "Failed to cancel task")
		return
	}

	// If the task is executing in THIS process, interrupt the live run (#508):
	// the governed loop halts at its next checkpoint, the sandbox/MCP client are
	// released by the run's defers, and the partial transcript persists. A task
	// running nowhere (pending) or in a prior process's orphaned lease simply
	// stays cancelled in the DB, exactly as before.
	if h.taskStopper != nil && h.taskStopper(taskID, who) {
		log.Printf("Task cancelled: %s (live run interrupted, stopped by %s)", taskID, logSafe(who)) //nolint:gosec // G706: taskID is a parsed uuid.UUID; who passes logSafe.
	} else {
		log.Printf("Task cancelled: %s (stopped by %s)", taskID, logSafe(who)) //nolint:gosec // G706: taskID is a parsed uuid.UUID; who passes logSafe.
	}
	writeJSON(w, http.StatusOK, task)
}

// stopLabel renders the cancelling principal for the who-stopped-it audit
// trail (#508): the authenticated username, the scoped API key's name, or
// "admin" for the bootstrap admin key.
func (p principal) stopLabel() string {
	switch {
	case p.user != nil && strings.TrimSpace(p.user.Username) != "":
		return p.user.Username
	case p.apiKey != nil:
		if strings.TrimSpace(p.apiKey.Name) != "" {
			return "api-key:" + p.apiKey.Name
		}
		return "api-key:" + p.apiKey.KeyID
	case p.isAdmin:
		return "admin"
	default:
		return "operator"
	}
}

// UpdateTask handles PUT /tasks/{task_id}
// Only tasks in pending or scheduled status can be edited.
func (h *Handlers) UpdateTask(w http.ResponseWriter, r *http.Request) {
	p := h.principalFromRequest(r)
	// Editing a task mutates work to be executed, so it requires the same
	// privilege as creating one — a read-only principal must not be able to
	// rewrite a task's prompt or selection.
	if !p.hasPermission(models.PermissionCreateTask) {
		writeError(w, http.StatusForbidden, "Insufficient permissions")
		return
	}

	taskIDStr := chi.URLParam(r, "task_id")
	taskID, err := uuid.Parse(taskIDStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid task ID")
		return
	}

	// Get existing task
	task, err := h.storage.GetTask(taskID)
	if err != nil || task == nil {
		writeError(w, http.StatusNotFound, "Task not found")
		return
	}

	// Only allow editing tasks that haven't started
	if task.Status != models.TaskStatusPending && task.Status != models.TaskStatusScheduled {
		writeError(w, http.StatusBadRequest, "Only pending or scheduled tasks can be edited")
		return
	}

	// Parse the update payload
	var tc models.TaskCreate
	if err := readJSON(r, &tc); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	// On edit, an omitted timezone means "keep the task's current zone" — not
	// "reset to the global default". Pre-fill from the existing task so
	// validateTaskCreate validates/keeps it instead of defaulting.
	if strings.TrimSpace(tc.Timezone) == "" {
		tc.Timezone = task.Timezone
	}

	if err := h.validateTaskCreate(&tc); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Same privilege boundary as create, but edit-shaped: a principal without
	// PermissionAdmin editing a task may keep an existing (admin-authorized)
	// gate — clients echo the record, modulo defaultable fields, hence the
	// normalized comparison — but may not add, change, or remove one. An
	// admin-permission principal's payload is authoritative (SetRunIf below),
	// so an admin edit that changes or removes the gate actually persists.
	canAuthorRunIf := p.hasPermission(models.PermissionAdmin)
	runIfChanged := !reflect.DeepEqual(tc.RunIf.Normalized(), task.RunIf.Normalized())
	if !canAuthorRunIf && runIfChanged {
		writeError(w, http.StatusForbidden, "run_if: a host-side pre-run gate can only be changed by an admin")
		return
	}
	// A gate is evaluated only at the scheduled→pending promotion
	// (models.RunIf's enforcement contract), and a pending task is already
	// past that point: honoring a new or changed gate would have the
	// dispatch-state recompute (models.DeriveDispatchState, applied in
	// UpdateEditableTask) yank the imminent dispatch back onto the scheduler
	// path. Refuse instead, so that change stays explicit — edit the gate
	// while the task is scheduled, or cancel and recreate it. Removing the
	// gate (tc.RunIf == nil) stays allowed: absence needs no evaluation point.
	if runIfChanged && tc.RunIf != nil && task.Status == models.TaskStatusPending {
		writeError(w, http.StatusConflict, "run_if: task is already pending dispatch, so its gate can no longer be evaluated for this run; change the gate while the task is scheduled, or cancel and recreate it")
		return
	}

	// If recurrence is changed, refresh the next scheduled run unless the user
	// explicitly chose a different schedule time in this edit.
	if tc.Recurrence != "" && tc.Recurrence != task.Recurrence {
		shouldRecalculateSchedule := tc.ScheduledFor == nil

		if !shouldRecalculateSchedule && tc.ScheduledFor != nil && task.ScheduledFor != nil {
			requested := tc.ScheduledFor.UTC().Truncate(time.Minute)
			existing := task.ScheduledFor.UTC().Truncate(time.Minute)
			if requested.Equal(existing) {
				shouldRecalculateSchedule = true
			}
		}

		if shouldRecalculateSchedule {
			schedule, err := cron.ParseStandard(tc.Recurrence)
			if err != nil {
				writeError(w, http.StatusBadRequest, "recurrence must be a standard 5-field cron expression")
				return
			}

			// Evaluate the cron expression in the task's own timezone (validated
			// above). scheduled_for is always stored as an absolute UTC instant.
			loc, lerr := time.LoadLocation(tc.Timezone)
			if lerr != nil {
				loc = h.storage.Location()
			}
			nextUTC := schedule.Next(time.Now().In(loc)).UTC()
			tc.ScheduledFor = &nextUTC
		}
	}

	// Apply updates transactionally: the storage layer re-locks the row and
	// re-checks that it is still editable, so a node leasing the task or a
	// cancellation between our read above and this write cannot be silently
	// overwritten (which would resurrect the task and clobber its lease).
	edit := storage.TaskEdit{
		Prompt:                 tc.Prompt,
		Title:                  tc.Title,
		Description:            tc.Description,
		Model:                  tc.Model,
		FallbackModel:          tc.FallbackModel,
		MaxIterations:          tc.MaxIterations,
		MCPSelection:           tc.MCPSelection,
		SetMCPSelection:        tc.MCPSelection != nil,
		CredentialAllowlist:    tc.CredentialAllowlist,
		SetCredentialAllowlist: tc.CredentialAllowlist != nil,
		LoopConfig:             tc.LoopConfig,
		SetLoopConfig:          tc.LoopConfig != nil,
		WorktreeConfig:         tc.WorktreeConfig,
		SetWorktreeConfig:      tc.WorktreeConfig != nil,
		RetryPolicy:            tc.RetryPolicy,
		SetRetryPolicy:         tc.RetryPolicy != nil,
		Priority:               tc.Priority,
		InstructionSelfImprove: tc.InstructionSelfImprove,
		AllowNetwork:           tc.AllowNetwork,
		CarryContext:           tc.CarryContext,
		AllowDelegation:        tc.DelegationAllowed(),
		ThinkingBudgetTokens:   tc.ThinkingBudgetTokens,
		Persona:                tc.Persona,
		ScheduledFor:           tc.ScheduledFor,
		Recurrence:             tc.Recurrence,
		RecurrenceUntil:        tc.RecurrenceUntil,
		RecurrenceRemaining:    tc.RecurrenceRemaining,
		Timezone:               tc.Timezone,
		Files:                  tc.Files,
		FileNames:              tc.FileNames,
		SetFiles:               tc.Files != nil,
		Tags:                   tc.Tags,
		SetTags:                tc.Tags != nil,
		// The payload is authoritative for run_if only for an admin-permission
		// principal (nil = remove the gate — PUT is full-replace and the web
		// client omits run_if when the command field is cleared). A non-admin's
		// echo already passed the normalized equality check above, so the stored
		// gate is kept byte-identical rather than rewritten from the echo.
		RunIf:         tc.RunIf,
		SetRunIf:      canAuthorRunIf,
		SandboxLimits: tc.SandboxLimits,
		// SLA config (#274) already passed ValidateSLA above; the web client
		// echoes the stored multipliers so a UI edit round-trips API-set values.
		ExpectedDurationMinutes: tc.ExpectedDurationMinutes,
		SLAWarnMultiplier:       tc.SLAWarnMultiplier,
		SLAFailMultiplier:       tc.SLAFailMultiplier,
	}

	updated, err := h.storage.UpdateEditableTask(r.Context(), taskID, edit)
	if err != nil {
		if errors.Is(err, storage.ErrTaskNotEditable) {
			writeError(w, http.StatusConflict, "Task is no longer editable (it may have started or been cancelled)")
			return
		}
		writeError(w, http.StatusInternalServerError, "Failed to update task")
		return
	}

	//nolint:gosec // G706: untrusted fields are sanitized via logSafe (strips CR/LF); gosec's taint tracker cannot see through the helper. updated.ID is a uuid.UUID.
	log.Printf("Task updated: %s (prompt: %.50s...)", updated.ID, logSafe(updated.Prompt))
	localizeTask(updated)
	writeJSON(w, http.StatusOK, updated)
}

// withAgentPool copies the (possibly cached) stats and stamps the LIVE
// agent-pool occupancy, so the stats cache can never freeze the agent
// numbers. Returns the input untouched when the pool isn't wired.
func (h *Handlers) withAgentPool(stats *models.DashboardStats) *models.DashboardStats {
	if h.agentPoolActive == nil || h.agentPoolSlots == nil {
		return stats
	}
	out := *stats
	active := h.agentPoolActive()
	slots := h.agentPoolSlots()
	out.ActiveAgents = &active
	out.AgentSlots = &slots
	return &out
}

// GetTagCatalogue handles GET /tasks/tags (#212): the distinct tags in use with
// per-tag task counts, busiest first. A read endpoint — group auth suffices.
func (h *Handlers) GetTagCatalogue(w http.ResponseWriter, r *http.Request) {
	catalogue, err := h.storage.ListTagCatalogue(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to load tag catalogue")
		return
	}
	writeJSON(w, http.StatusOK, catalogue)
}

// GetSLAReport handles GET /sla-report (#274): the per-prompt SLA actuals
// (p50/p95 actual duration + breach rate) over a window. The window is optional
// via ?days= (default 7, clamped to [1, 90] by the storage layer).
//
// Admin-only, but reachable from the web UI: it is registered behind
// AdminOrUserAuthMiddleware (which resolves the caller into a principal) and
// gated HERE on PermissionAdmin (#458). The admin API key OR an admin-role user
// (the role the Next proxy's header-trust/bearer path conveys) passes; a
// non-admin member gets 403. This is the only gate that lets a web-authenticated
// admin reach the report — the proxy can never send the admin X-API-Key that the
// bare AdminAuthMiddleware demanded, so the report was previously unreachable
// from the dashboard for every user. The report is GLOBAL (all tasks) while a
// member may be scoped, so exposing it to a scoped non-admin would leak the
// existence/volume/latency of work outside their scope — hence admin-only.
func (h *Handlers) GetSLAReport(w http.ResponseWriter, r *http.Request) {
	if !h.principalFromRequest(r).hasPermission(models.PermissionAdmin) {
		writeError(w, http.StatusForbidden, "Admin access required")
		return
	}
	days := 7
	if raw := r.URL.Query().Get("days"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			days = n
		}
	}
	report, err := h.storage.GetSLAReport(r.Context(), days)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to load SLA report")
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// tagMutation is the POST /tasks/{task_id}/tags body: tags to add and/or remove.
type tagMutation struct {
	Add    []string `json:"add"`
	Remove []string `json:"remove"`
}

// UpdateTaskTags handles POST /tasks/{task_id}/tags (#212): atomically add and/or
// remove tags on a task. Mutating, so it requires the create-task privilege.
func (h *Handlers) UpdateTaskTags(w http.ResponseWriter, r *http.Request) {
	p := h.principalFromRequest(r)
	if !p.hasPermission(models.PermissionCreateTask) {
		writeError(w, http.StatusForbidden, "Insufficient permissions")
		return
	}

	taskID, err := uuid.Parse(chi.URLParam(r, "task_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid task ID")
		return
	}

	task, err := h.storage.GetTask(taskID)
	if err != nil || task == nil {
		writeError(w, http.StatusNotFound, "Task not found")
		return
	}
	var body tagMutation
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	// Normalize add/remove independently so a malformed tag is rejected before any
	// mutation; the storage layer re-validates the merged set.
	add, err := models.NormalizeAndValidateTags(body.Add)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("add tags: %v", err))
		return
	}
	remove, err := models.NormalizeAndValidateTags(body.Remove)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("remove tags: %v", err))
		return
	}

	updated, err := h.storage.UpdateTaskTags(r.Context(), taskID, add, remove)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to update tags: %v", err))
		return
	}
	localizeTask(updated)
	writeJSON(w, http.StatusOK, updated)
}

// taskRerunOverrides is the optional subset of fields a re-run / clone may change
// vs the source task (#270). Pointer fields → nil means "inherit from source".
type taskRerunOverrides struct {
	Prompt          *string `json:"prompt,omitempty"`
	Model           *string `json:"model,omitempty"`
	FallbackModel   *string `json:"fallback_model,omitempty"`
	MaxIterations   *int    `json:"max_iterations,omitempty"`
	Priority        *int    `json:"priority,omitempty"`
	AllowNetwork    *bool   `json:"allow_network,omitempty"`
	AllowDelegation *bool   `json:"allow_delegation,omitempty"`
	// ThinkingBudgetTokens per-task thinking override (#220). Present sets it;
	// a negative value clears back to inherit-global; absent leaves it.
	ThinkingBudgetTokens *int     `json:"thinking_budget_tokens,omitempty"`
	Description          *string  `json:"description,omitempty"`
	Title                *string  `json:"title,omitempty"`
	Tags                 []string `json:"tags,omitempty"`
	Persona              *string  `json:"persona,omitempty"`
}

// taskRerunRequest is the (optional) body of POST /tasks/{id}/rerun|clone.
type taskRerunRequest struct {
	Overrides taskRerunOverrides `json:"overrides"`
}

// RerunTask handles POST /tasks/{task_id}/rerun (#270): create a NEW one-time
// task copied from the source (scheduled_for=now, recurrence cleared), with
// optional field overrides. The original is untouched.
func (h *Handlers) RerunTask(w http.ResponseWriter, r *http.Request) {
	h.rerunOrClone(w, r, false)
}

// CloneTask handles POST /tasks/{task_id}/clone (#270): like rerun, but preserves
// the source's recurrence (computing the next fire time for a cron task).
func (h *Handlers) CloneTask(w http.ResponseWriter, r *http.Request) {
	h.rerunOrClone(w, r, true)
}

// rerunOrClone is the shared body for RerunTask/CloneTask. keepRecurrence=false
// (rerun) clears recurrence and fires immediately; true (clone) keeps it.
func (h *Handlers) rerunOrClone(w http.ResponseWriter, r *http.Request, keepRecurrence bool) {
	p := h.principalFromRequest(r)
	if !p.hasPermission(models.PermissionCreateTask) {
		writeError(w, http.StatusForbidden, "Insufficient permissions")
		return
	}

	taskID, err := uuid.Parse(chi.URLParam(r, "task_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid task ID")
		return
	}
	source, err := h.storage.GetTask(taskID)
	if err != nil || source == nil {
		writeError(w, http.StatusNotFound, "Task not found")
		return
	}
	// Own-rows visibility (#1082): the copy inherits the source's prompt and
	// config, so re-running/cloning a task you cannot see would read it.
	if !taskVisibleToPrincipal(p, source) {
		writeError(w, http.StatusForbidden, "Tasks are private to their creator")
		return
	}
	// The body is optional; only decode when present (rerun-with-no-changes sends none).
	var req taskRerunRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := readJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid request body")
			return
		}
	}

	loc := h.storage.Location()
	tc, berr := buildRerunTaskCreate(source, keepRecurrence, req.Overrides, loc)
	if berr != nil {
		writeError(w, http.StatusBadRequest, berr.Error())
		return
	}

	if err := h.validateTaskCreate(&tc); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	newTask := models.NewTask(tc)
	newTask.SourceTaskID = &source.ID
	newTask.CreatedBy = p.ownerID()

	if _, err := h.storage.AddTaskWithContext(r.Context(), newTask); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to create task")
		return
	}
	verb := "re-run"
	if keepRecurrence {
		verb = "clone"
	}
	//nolint:gosec // G706: IDs are uuid.UUID values; their String() is canonical hex+dashes, no CR/LF.
	log.Printf("Task %s created (%s of %s)", newTask.ID, verb, source.ID)
	localizeTask(newTask)
	writeJSON(w, http.StatusCreated, newTask)
}

// buildRerunTaskCreate constructs the TaskCreate recipe for a re-run (#270) from
// a source task. keepRecurrence=false (rerun) clears recurrence and runs
// immediately; true (clone) preserves a cron recurrence (recomputing the next
// fire in the task's timezone, falling back to fallbackLoc). Overrides are then
// applied. It is pure (no h/HTTP) so the scheduling logic is unit-testable.
//
// IMPORTANT: an immediate run uses ScheduledFor=nil — the codebase's "run now"
// convention (a fresh pending task the worker claims at once). Setting &now would
// be rejected by validateTaskCreate's "scheduled time cannot be in the past"
// check, which re-samples a strictly-later now. A gated source is the one
// exception to "claims at once": TaskToCreate carries run_if, and NewTask parks
// any gated cron task scheduled-for-now so the scheduler evaluates the gate
// before dispatch (models.RunIf's enforcement contract) — a rerun must not be a
// path around the condition the gate exists to enforce.
func buildRerunTaskCreate(source *models.Task, keepRecurrence bool, o taskRerunOverrides, fallbackLoc *time.Location) (models.TaskCreate, error) {
	tc := models.TaskToCreate(source)
	// A copy is never the named DEFINITION. Name is the import/export identity
	// key and carries a partial unique index on non-empty names, so inheriting
	// the source's name collides with the source row still in the table and the
	// insert fails (a 500 on every re-run of a named task). Storage's recurrence
	// chain clears Name for exactly this reason; the copy paths must match.
	tc.Name = ""
	if keepRecurrence && strings.TrimSpace(tc.Recurrence) != "" {
		schedule, perr := cron.ParseStandard(tc.Recurrence)
		if perr != nil {
			return tc, fmt.Errorf("source task has an invalid recurrence")
		}
		loc := fallbackLoc
		if l, lerr := time.LoadLocation(tc.Timezone); lerr == nil && l != nil {
			loc = l
		}
		if loc == nil {
			loc = time.UTC
		}
		next := schedule.Next(time.Now().In(loc)).UTC()
		tc.ScheduledFor = &next
	} else {
		tc.ScheduledFor = nil // immediate run-now
		if !keepRecurrence {
			tc.Recurrence = "" // rerun is one-time
		}
	}
	applyRerunOverrides(&tc, o)
	return tc, nil
}

// applyRerunOverrides mutates tc with any non-nil override fields (#270). Tags,
// when provided (non-nil, possibly empty), replace the inherited set.
func applyRerunOverrides(tc *models.TaskCreate, o taskRerunOverrides) {
	if o.Prompt != nil {
		tc.Prompt = *o.Prompt
	}
	if o.Model != nil {
		tc.Model = o.Model
	}
	if o.FallbackModel != nil {
		tc.FallbackModel = o.FallbackModel
	}
	if o.MaxIterations != nil {
		tc.MaxIterations = o.MaxIterations
	}
	if o.Priority != nil {
		tc.Priority = *o.Priority
	}
	if o.AllowNetwork != nil {
		tc.AllowNetwork = *o.AllowNetwork
	}
	if o.AllowDelegation != nil {
		tc.AllowDelegation = o.AllowDelegation
	}
	if o.ThinkingBudgetTokens != nil {
		if *o.ThinkingBudgetTokens < 0 {
			tc.ThinkingBudgetTokens = nil // clear → inherit the global default
		} else {
			v := *o.ThinkingBudgetTokens
			tc.ThinkingBudgetTokens = &v
		}
	}
	if o.Description != nil {
		tc.Description = *o.Description
	}
	if o.Title != nil {
		tc.Title = *o.Title
	}
	if o.Tags != nil {
		tc.Tags = o.Tags
	}
	if o.Persona != nil {
		tc.Persona = *o.Persona
	}
}

// GetLogs handles GET /logs/{task_id}. Authorization is the shared transcript
// gate (see log_authz.go): view_logs plus per-task ownership, or the explicit
// fleet-wide view_all_logs.
func (h *Handlers) GetLogs(w http.ResponseWriter, r *http.Request) {
	task, ok := h.logReadableTask(w, r, "Logs not found for this task")
	if !ok {
		return
	}

	session, err := h.storage.GetLog(task.ID)
	if err != nil || session == nil {
		writeError(w, http.StatusNotFound, "Logs not found for this task")
		return
	}

	writeJSON(w, http.StatusOK, session)
}

// logHistoryTaskForRequest parses the path task id and enforces exactly the
// GetLogs gate (the shared transcript gate in log_authz.go), so the per-attempt
// history endpoints can never leak more than the latest-log one.
func (h *Handlers) logHistoryTaskForRequest(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	task, ok := h.logReadableTask(w, r, "Logs not found for this task")
	if !ok {
		return uuid.Nil, false
	}
	return task.ID, true
}

// GetLogHistory handles GET /logs/{task_id}/history — the task's superseded
// transcripts (per-attempt run log history), newest first, metadata only.
func (h *Handlers) GetLogHistory(w http.ResponseWriter, r *http.Request) {
	taskID, ok := h.logHistoryTaskForRequest(w, r)
	if !ok {
		return
	}
	entries, err := h.storage.ListRunLogHistory(r.Context(), taskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list log history")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// GetLogHistoryEntry handles GET /logs/{task_id}/history/{entry_id} — one
// superseded transcript, in the same LogSession shape GetLogs returns.
func (h *Handlers) GetLogHistoryEntry(w http.ResponseWriter, r *http.Request) {
	taskID, ok := h.logHistoryTaskForRequest(w, r)
	if !ok {
		return
	}
	entryID, err := strconv.ParseInt(chi.URLParam(r, "entry_id"), 10, 64)
	if err != nil || entryID <= 0 {
		writeError(w, http.StatusBadRequest, "Invalid history entry ID")
		return
	}
	session, err := h.storage.GetRunLogEntry(r.Context(), taskID, entryID)
	if err != nil || session == nil {
		writeError(w, http.StatusNotFound, "Log history entry not found")
		return
	}
	writeJSON(w, http.StatusOK, session)
}

// API Key Management Endpoints

// CreateAPIKey handles POST /keys
func (h *Handlers) CreateAPIKey(w http.ResponseWriter, r *http.Request) {
	var keyCreate models.APIKeyCreate
	if err := readJSON(r, &keyCreate); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	// The per-key spending caps are retired: nothing ever fed their accumulator,
	// so a cap set here was silently unenforced. Refuse the field rather than
	// accept it — an operator who believes a key is capped at $50/day and is
	// wrong is worse off than one who gets an error naming the replacement.
	// Checked BEFORE creating the key so a rejected request mints nothing.
	if keyCreate.MaxCostPerDayUSD != nil || keyCreate.MaxCostPerMonthUSD != nil {
		writeError(w, http.StatusBadRequest,
			"max_cost_per_day_usd/max_cost_per_month_usd are no longer supported; create a rolling budget instead: POST /admin/budgets {\"scope\":\"key\",\"principal_id\":\"<key_id>\",\"window\":\"day|week|month\",\"hard_usd\":…}")
		return
	}

	// Validate the optional task-urgency ceiling (#230) BEFORE creating the key,
	// so a bad value never leaves a half-created (uncapped) key behind.
	if keyCreate.MaxPriority != nil && (*keyCreate.MaxPriority < models.PriorityMin || *keyCreate.MaxPriority > models.PriorityMax) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("max_priority must be between %d and %d", models.PriorityMin, models.PriorityMax))
		return
	}

	// An explicit permission set (#980) is the legacy path's escape hatch for a
	// grant no role expresses. Reject the ambiguous combinations up front rather
	// than picking a winner silently — CreateKey prefers role over permissions,
	// and a typed key ignores both, so a caller that sent two of them would get a
	// key with permissions it did not ask for.
	if len(keyCreate.Permissions) > 0 {
		switch {
		case keyCreate.Type != "":
			writeError(w, http.StatusBadRequest, "permissions cannot be combined with type; a typed key derives its permissions from its type")
			return
		case keyCreate.Role != nil:
			writeError(w, http.StatusBadRequest, "permissions cannot be combined with role; set one or the other")
			return
		}
		for _, perm := range keyCreate.Permissions {
			if !perm.Valid() {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown permission %q", perm))
				return
			}
		}
	}

	var (
		key    *apikeys.APIKey
		rawKey string
		err    error
	)
	if keyCreate.Type != "" {
		// Typed-key path (#190): the type determines the permission set; reject an
		// unknown type, and require a webhook key to be scoped to ≥1 valid slug.
		kt := apikeys.KeyType(keyCreate.Type)
		if !kt.Valid() {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown key type %q (want admin|task|webhook|readonly)", keyCreate.Type))
			return
		}
		if kt == apikeys.KeyTypeWebhook {
			if len(keyCreate.AllowedTriggerSlugs) == 0 {
				writeError(w, http.StatusBadRequest, "webhook key requires at least one allowed_trigger_slugs entry")
				return
			}
			for _, s := range keyCreate.AllowedTriggerSlugs {
				if !triggerSlugShape.MatchString(s) {
					writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid trigger slug %q", s))
					return
				}
			}
		}
		key, rawKey, err = h.apiKeys.CreateTypedKey(
			keyCreate.Name,
			kt,
			keyCreate.AllowedTriggerSlugs,
			keyCreate.RateLimit,
			keyCreate.ExpiresInDays,
			keyCreate.Description,
		)
	} else {
		// Legacy role-based path: mints an untyped sk- key. Unchanged except that
		// an explicit (validated, role-free) permission set is now honored.
		key, rawKey, err = h.apiKeys.CreateKey(
			keyCreate.Name,
			keyCreate.Permissions,
			keyCreate.Role,
			keyCreate.RateLimit,
			keyCreate.ExpiresInDays,
			keyCreate.Description,
		)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to create API key")
		return
	}

	// Apply the optional task-urgency ceiling (#230), validated above.
	if keyCreate.MaxPriority != nil {
		if err := h.apiKeys.SetMaxPriority(key.KeyID, keyCreate.MaxPriority); err != nil {
			log.Printf("Warning: failed to set max_priority on new key %s: %v", key.KeyID, err)
		}
	}

	log.Printf("Created API key: %s (%s)", key.KeyID, key.Name)

	resp := key.ToResponse()
	writeJSON(w, http.StatusOK, models.APIKeyCreated{
		APIKeyResponse: resp,
		APIKey:         rawKey,
	})
}

// ListAPIKeys handles GET /keys
func (h *Handlers) ListAPIKeys(w http.ResponseWriter, _ *http.Request) {
	keys := h.apiKeys.ListKeys()
	responses := make([]models.APIKeyResponse, len(keys))
	for i, key := range keys {
		responses[i] = key.ToResponse()
	}

	writeJSON(w, http.StatusOK, responses)
}

// GetAuditLog handles GET /keys/audit
func (h *Handlers) GetAuditLog(w http.ResponseWriter, r *http.Request) {
	keyID := r.URL.Query().Get("key_id")
	action := r.URL.Query().Get("action")
	hours := 24
	if hr := r.URL.Query().Get("hours"); hr != "" {
		parsed, err := strconv.Atoi(hr)
		if err != nil || parsed < 1 || parsed > 24*365 {
			writeError(w, http.StatusBadRequest, "Invalid hours parameter (must be 1-8760)")
			return
		}
		hours = parsed
	}
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		parsed, err := strconv.Atoi(l)
		if err != nil || parsed < 1 || parsed > 1000 {
			writeError(w, http.StatusBadRequest, "Invalid limit parameter (must be 1-1000)")
			return
		}
		limit = parsed
	}

	var since *time.Time
	if hours > 0 {
		t := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)
		since = &t
	}

	var keyIDPtr, actionPtr *string
	if keyID != "" {
		keyIDPtr = &keyID
	}
	if action != "" {
		actionPtr = &action
	}

	entries := h.apiKeys.GetAuditLog(keyIDPtr, actionPtr, since, limit)
	writeJSON(w, http.StatusOK, models.AuditLogResponse{
		Entries: entries,
		Total:   len(entries),
	})
}

// GetAPIKey handles GET /keys/{key_id}
func (h *Handlers) GetAPIKey(w http.ResponseWriter, r *http.Request) {
	keyID := chi.URLParam(r, "key_id")
	key := h.apiKeys.GetKey(keyID)
	if key == nil {
		writeError(w, http.StatusNotFound, "API key not found")
		return
	}

	writeJSON(w, http.StatusOK, key.ToResponse())
}

// RotateAPIKey handles POST /keys/{key_id}/rotate
func (h *Handlers) RotateAPIKey(w http.ResponseWriter, r *http.Request) {
	keyID := chi.URLParam(r, "key_id")
	gracePeriodHours := 24
	if g := r.URL.Query().Get("grace_period_hours"); g != "" {
		if parsed, err := strconv.Atoi(g); err == nil {
			gracePeriodHours = parsed
		}
	}

	key, rawKey, err := h.apiKeys.RotateKey(keyID, gracePeriodHours)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	//nolint:gosec // G706: keyID is sanitized via logSafe (strips CR/LF); gosec's taint tracker cannot see through the helper.
	log.Printf("Rotated API key: %s", logSafe(keyID))

	resp := key.ToResponse()
	writeJSON(w, http.StatusOK, models.APIKeyRotated{
		APIKeyResponse:   resp,
		APIKey:           rawKey,
		GracePeriodHours: gracePeriodHours,
	})
}

// RevokeAPIKey handles POST /keys/{key_id}/revoke
func (h *Handlers) RevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	keyID := chi.URLParam(r, "key_id")
	if err := h.apiKeys.RevokeKey(keyID); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	//nolint:gosec // G706: keyID is sanitized via logSafe (strips CR/LF); gosec's taint tracker cannot see through the helper.
	log.Printf("Revoked API key: %s", logSafe(keyID))

	key := h.apiKeys.GetKey(keyID)
	if key == nil {
		writeError(w, http.StatusNotFound, "API key not found")
		return
	}

	writeJSON(w, http.StatusOK, key.ToResponse())
}

// DeleteAPIKey handles DELETE /keys/{key_id}
func (h *Handlers) DeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	keyID := chi.URLParam(r, "key_id")
	if err := h.apiKeys.DeleteKey(keyID); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	//nolint:gosec // G706: keyID is sanitized via logSafe (strips CR/LF); gosec's taint tracker cannot see through the helper.
	log.Printf("Deleted API key: %s", logSafe(keyID))
	writeJSON(w, http.StatusOK, models.DeleteKeyResponse{
		Deleted: true,
		KeyID:   keyID,
	})
}

// Dashboard Endpoints

// GetDashboardStats handles GET /dashboard/stats
func (h *Handlers) GetDashboardStats(w http.ResponseWriter, r *http.Request) {
	user := GetUserFromContext(r.Context())
	if user != nil && !userHasPermission(user, models.PermissionViewTasks) {
		writeError(w, http.StatusForbidden, "Insufficient permissions")
		return
	}

	// One box, one task table: every principal that clears the permission check
	// above sees the same counts, so the stats are cached under a single key.
	const cacheKey = "global"
	if cached, ok := h.statsCache.Get(cacheKey); ok {
		writeJSON(w, http.StatusOK, h.withAgentPool(cached))
		return
	}

	stats, err := h.storage.GetDashboardStats()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to get dashboard stats")
		return
	}

	// Update cache
	// 5s TTL: still collapses a burst of dashboard polls into one DB round of
	// count queries, but no longer makes the stat cards lag the world by up to
	// a minute — the web dashboard polls every 5s while work is in flight.
	h.statsCache.Set(cacheKey, stats, 5*time.Second)

	writeJSON(w, http.StatusOK, h.withAgentPool(stats))
}

// GetCurrentUser handles GET /api/me. It sits behind AdminOrUserAuthMiddleware,
// so it returns 200 for any valid credential (Elcano cookie, bearer token, or
// API key) and 401 otherwise. The dashboard uses it on page load to detect a
// cookie session — cookie users have no bearer token in localStorage, so the
// SPA would otherwise show the login card despite being authenticated.
func (h *Handlers) GetCurrentUser(w http.ResponseWriter, r *http.Request) {
	user := GetUserFromContext(r.Context())
	if user == nil {
		// Authenticated via an API key (no user record); still signed in.
		writeJSON(w, http.StatusOK, map[string]interface{}{"authenticated": true})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"authenticated": true,
		"username":      user.Username,
		"role":          user.Role,
	})
}

// GetDashboardConfig handles GET /api/config.
//
// server_time is the orchestrator's own clock, formatted in ITS configured
// location so the offset travels with it. The dashboard clock needs it for two
// reasons the timezone name alone cannot cover: an operator's browser clock may
// be wrong (rendering "now" locally would then display a confident lie), and a
// deployment may be configured with a zone the browser's ICU data does not
// know, in which case the embedded offset is still enough to render the time.
// A client computes its skew once from this and ticks locally.
func (h *Handlers) GetDashboardConfig(w http.ResponseWriter, _ *http.Request) {
	loc := h.storage.Location()
	if loc == nil {
		loc = time.UTC
	}
	config := map[string]interface{}{
		"version":     h.config.Version,
		"timezone":    h.config.Timezone,
		"server_time": time.Now().In(loc).Format(time.RFC3339),
		// The zone a scheduled task lands in when its create request names none
		// — distinct from the server clock above, and the one that actually
		// decides when "0 9 * * *" fires.
		"default_task_timezone": h.defaultTaskTimezone(),
	}

	writeJSON(w, http.StatusOK, config)
}

// Health Check

// HealthCheck handles GET /health
func (h *Handlers) HealthCheck(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, models.HealthResponse{
		Status:    "healthy",
		Version:   h.config.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
}

func userHasPermission(user *models.User, permission models.Permission) bool {
	if user == nil {
		return false
	}
	perms := models.RolePermissions[user.Role]
	for _, perm := range perms {
		if perm == models.PermissionAdmin || perm == permission {
			return true
		}
	}
	return false
}

// principal is the resolved identity for a request authenticated through
// AdminOrUserAuthMiddleware. It unifies the three credential types — the admin
// API key, a scoped API key, and a user token/Elcano cookie — so that handlers
// enforce permissions uniformly. Before this existed, permission checks were
// gated on `user != nil`, which silently granted API keys unrestricted,
// cross-tenant access (and let read-only keys mutate tasks).
type principal struct {
	user    *models.User
	apiKey  *apikeys.APIKey
	isAdmin bool // authenticated with the admin API key
}

func (h *Handlers) principalFromRequest(r *http.Request) principal {
	return principal{
		user:    GetUserFromContext(r.Context()),
		apiKey:  GetAPIKeyFromContext(r.Context()),
		isAdmin: h.verifyAdminKey(r),
	}
}

// hasPermission reports whether the principal is allowed to perform perm.
func (p principal) hasPermission(perm models.Permission) bool {
	switch {
	case p.isAdmin:
		return true
	case p.user != nil:
		return userHasPermission(p.user, perm)
	case p.apiKey != nil:
		return p.apiKey.HasPermission(perm)
	default:
		return false
	}
}

// ownerID returns the user ID used for creator-based task visibility, or nil
// for API-key principals (key-created tasks have no creator user).
func (p principal) ownerID() *uuid.UUID {
	if p.user != nil {
		return &p.user.ID
	}
	return nil
}

// ownsTask is the narrow non-admin stop authorization: only user-created tasks
// participate. API-key principals continue to require PermissionCancelTask,
// even when a task records their key id, so a read-only key never gains a
// mutation through ownership.
func (p principal) ownsTask(task *models.Task) bool {
	ownerID := p.ownerID()
	return task != nil && ownerID != nil && task.CreatedBy != nil && *ownerID == *task.CreatedBy
}

// populateCreatedByUsernames populates the CreatedByUsername field for each task.
func (h *Handlers) populateCreatedByUsernames(ctx context.Context, tasks []*models.Task) error {
	// Collect unique CreatedBy UUIDs
	userIDs := make([]uuid.UUID, 0)
	seen := make(map[uuid.UUID]bool)
	for _, task := range tasks {
		if task.CreatedBy != nil && !seen[*task.CreatedBy] {
			userIDs = append(userIDs, *task.CreatedBy)
			seen[*task.CreatedBy] = true
		}
	}

	if len(userIDs) == 0 {
		return nil
	}

	// Fetch usernames using request context for proper cancellation
	usernames, err := h.storage.GetUsersByIDsWithContext(ctx, userIDs)
	if err != nil {
		return err
	}

	// Populate tasks
	for _, task := range tasks {
		if task.CreatedBy != nil {
			if username, ok := usernames[*task.CreatedBy]; ok {
				task.CreatedByUsername = &username
			}
		}
	}

	return nil
}

// PipelineMetrics handles GET /admin/pipeline-metrics (#543): the sensor
// behind data-driven optimization decisions (#505's reopen criteria). It
// derives tool-pipeline shape — tool turns, distinct tools, tokens, latency —
// from the session logs fleet already persists, so it works retroactively.
//
// Retention does NOT bound this table by default (FLEET_RUN_LOG_RETENTION_DAYS
// <= 0 leaves pruning off), so the scan is keyset-paginated: one page of
// payloads at a time, a running aggregate, and only the most recent ?runs=
// summaries retained (default 100, max 500). The aggregate still covers
// every stored log; peak memory is O(page + runs limit), not O(table) (#1122).
// Admin-gated by the route group.
func (h *Handlers) PipelineMetrics(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("runs"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 500 {
		limit = 500
	}
	acc := models.NewPipelineMetricsAccumulator()
	recent := models.NewRecentRuns(limit)
	err := h.storage.ForEachLog(r.Context(), func(taskID uuid.UUID, session *models.LogSession) error {
		m := models.ComputePipelineMetrics(taskID.String(), session)
		acc.Add(m)
		recent.Add(m)
		return nil
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to read session logs")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"aggregate": acc.Result(),
		"runs":      recent.Items(),
	})
}
