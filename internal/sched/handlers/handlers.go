// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

// Package handlers provides HTTP handlers for the sched API.
package handlers

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/robfig/cron/v3"

	"github.com/ElcanoTek/fleet/internal/ratelimit"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// Config holds the handler configuration.
type Config struct {
	OrchestratorURL   string
	AdminAPIKey       string
	RegistrationToken string
	Version           string
	DataDir           string
	Timezone          string
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
	// one (CUTLASS_TASK_MAX_ITERATIONS, then MAX_ITERATIONS).
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

	// Rate limiting for registration
	regRateLimiter *rateLimiter
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

	// Cache for file checksums to avoid repeated disk I/O
	checksumCache *checksumCache

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

	// taskTemplates returns the read-only task-template catalog the task-create UI
	// renders as "new task from a template". Injected by cmd/fleet via
	// SetTaskTemplateProvider from the loaded client bundle; nil → empty catalog.
	// See task_templates.go.
	taskTemplates taskTemplateProvider

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

// checksumCache caches file checksums to avoid repeated disk I/O.
type checksumCache struct {
	mu    sync.RWMutex
	store map[string]string
}

func newChecksumCache() *checksumCache {
	return &checksumCache{
		store: make(map[string]string),
	}
}

func (c *checksumCache) Get(filename string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	val, ok := c.store[filename]
	return val, ok
}

func (c *checksumCache) Set(filename, checksum string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store[filename] = checksum
}

func (c *checksumCache) Delete(filename string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.store, filename)
}

// Clear removes all entries from the cache.
func (c *checksumCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store = make(map[string]string)
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
	taskScheduleMaxYears    = 5
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
		regRateLimiter:   newRateLimiter(10, time.Minute), // 10 registrations per minute per IP
		loginRateLimiter: newRateLimiter(20, time.Minute), // 20 logins per minute per IP
		taskKeyRL:        ratelimit.New(cfg.SchedRateLimitPerMinute, cfg.SchedRateLimitPerDay),
		taskGlobalRL:     ratelimit.New(cfg.SchedGlobalRateLimitPerMinute, 0),
		taskRLCounter:    newRateLimitCounter(),
		checksumCache:    newChecksumCache(),
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
	apiKey := r.Header.Get("X-API-Key")
	// Hash inputs to prevent length-deduction timing attacks before constant-time comparison
	apiKeyHash := sha256.Sum256([]byte(apiKey))
	expectedKeyHash := sha256.Sum256([]byte(h.config.AdminAPIKey))
	return subtle.ConstantTimeCompare(apiKeyHash[:], expectedKeyHash[:]) == 1
}

func (h *Handlers) verifyNodeKey(r *http.Request) (*models.Node, error) {
	apiKey := r.Header.Get("X-API-Key")
	if apiKey == "" {
		authHeader := r.Header.Get("Authorization")
		if len(authHeader) > 7 && authHeader[:7] == "Bearer " {
			apiKey = authHeader[7:]
		}
	}
	if apiKey == "" {
		return nil, fmt.Errorf("missing API key")
	}
	node, err := h.storage.GetNodeByAPIKey(apiKey)
	if err != nil || node == nil {
		return nil, fmt.Errorf("invalid node API key")
	}
	return node, nil
}

func (h *Handlers) verifyRegistrationToken(r *http.Request) error {
	if h.config.RegistrationToken == "" {
		return fmt.Errorf("registration is disabled")
	}
	token := r.Header.Get("X-Registration-Token")
	// Hash inputs to prevent length-deduction timing attacks before constant-time comparison
	tokenHash := sha256.Sum256([]byte(token))
	expectedHash := sha256.Sum256([]byte(h.config.RegistrationToken))
	if subtle.ConstantTimeCompare(tokenHash[:], expectedHash[:]) != 1 {
		return fmt.Errorf("invalid registration token")
	}
	return nil
}

// Node Management Endpoints

// RegisterNode handles POST /register
func (h *Handlers) RegisterNode(w http.ResponseWriter, r *http.Request) {
	var reg models.NodeRegistration
	if err := readJSON(r, &reg); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	// Determine the node name
	nodeName := reg.Hostname
	if reg.Name != nil {
		nodeName = *reg.Name
	}

	// Check for re-registration: if a node with the same name exists, update it
	existingNode, _ := h.storage.GetNodeByName(nodeName)
	if existingNode != nil {
		// Prevent re-registration while node has an active task
		if existingNode.CurrentTaskID != nil {
			writeError(w, http.StatusConflict, "Node has an active task and cannot be re-registered. Wait for the task to complete or cancel it first.")
			return
		}

		// Re-registration: update the existing node with key rotation grace period
		now := time.Now().UTC()

		// Preserve the old API key for the grace period. Copy it into a local
		// first: taking the address of existingNode.APIKey would alias the very
		// field we overwrite below, so PreviousAPIKey would end up pointing at
		// the new key and the grace-period lookup could never match the old one.
		// Note: existingNode.APIKey is already hashed in the database.
		oldAPIKey := existingNode.APIKey
		existingNode.PreviousAPIKey = &oldAPIKey
		existingNode.KeyRotatedAt = &now

		// Generate new API key
		newAPIKey := uuid.New().String()
		existingNode.Hostname = reg.Hostname
		existingNode.OSType = reg.OSType
		existingNode.Status = models.NodeStatusIdle
		existingNode.LastHeartbeat = now
		existingNode.APIKey = newAPIKey

		if _, err := h.storage.UpdateNode(existingNode); err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to update node")
			return
		}

		// Return the unhashed new API key to the client
		existingNode.APIKey = newAPIKey
		log.Printf("Node re-registered: %s (name: %s) - old key valid for %v", existingNode.ID, existingNode.Name, models.KeyRotationGracePeriod)
		writeJSON(w, http.StatusOK, existingNode)
		return
	}

	// New registration
	node := models.NewNode(reg)
	log.Printf("Registering new node: %s", node.Name)

	if _, err := h.storage.AddNode(node); err != nil {
		// A concurrent registration of the same name won the race to insert.
		// The unique constraint did its job; tell the runner to retry, at which
		// point it will find the existing row and take the re-registration path.
		if errors.Is(err, db.ErrDuplicateNodeName) {
			writeError(w, http.StatusConflict, "Node name was just registered by a concurrent request; please retry")
			return
		}
		writeError(w, http.StatusInternalServerError, "Failed to register node")
		return
	}

	log.Printf("Node registered: %s (name: %s)", node.ID, node.Name)
	writeJSON(w, http.StatusOK, node)
}

// ListNodes handles GET /nodes
// Requires pagination with ?limit=N&offset=M query parameters.
// Returns a PaginatedResponse with total count.
func (h *Handlers) ListNodes(w http.ResponseWriter, r *http.Request) {
	p := h.principalFromRequest(r)
	if !p.hasPermission(models.PermissionViewNodes) {
		writeError(w, http.StatusForbidden, "Insufficient permissions")
		return
	}
	user := p.user

	// Parse pagination parameters (default: limit=100, offset=0)
	limit := 100
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

	var nodes []*models.Node
	var total int
	var err error

	// Visibility rules:
	//   - Admin key / admin-role user: see all nodes.
	//   - Any principal with scopes (user or API key): see only matching nodes.
	//   - A non-admin user with no scopes: see nothing (avoids leaking the whole
	//     fleet to an unscoped client account).
	//   - A scoped API key with no patterns is unrestricted by design, so it
	//     falls into the admin branch below.
	scopes := p.scopes()
	switch {
	case p.isAdmin || (user != nil && userHasPermission(user, models.PermissionAdmin)) || (p.apiKey != nil && len(scopes) == 0):
		if len(scopes) > 0 {
			nodes, total, err = h.storage.GetNodesScopedPaginated(limit, offset, scopes)
		} else {
			nodes, total, err = h.storage.GetAllNodesPaginated(limit, offset)
		}
	case len(scopes) > 0:
		nodes, total, err = h.storage.GetNodesScopedPaginated(limit, offset, scopes)
	default:
		// Non-admin with no scopes -> no access (empty list).
		nodes = []*models.Node{}
		total = 0
		err = nil
	}

	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list nodes")
		return
	}

	writeJSON(w, http.StatusOK, models.PaginatedResponse{
		Data:   nodes,
		Total:  total,
		Limit:  limit,
		Offset: offset,
	})
}

// GetNode handles GET /nodes/{node_id}
func (h *Handlers) GetNode(w http.ResponseWriter, r *http.Request) {
	p := h.principalFromRequest(r)
	if !p.hasPermission(models.PermissionViewNodes) {
		writeError(w, http.StatusForbidden, "Insufficient permissions")
		return
	}

	nodeIDStr := chi.URLParam(r, "node_id")
	nodeID, err := uuid.Parse(nodeIDStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid node ID")
		return
	}

	node, err := h.storage.GetNode(nodeID)
	if err != nil || node == nil {
		writeError(w, http.StatusNotFound, "Node not found")
		return
	}

	if scopes := p.scopes(); len(scopes) > 0 && !scopesMatchNode(scopes, node.Name) {
		writeError(w, http.StatusForbidden, "Node not within allowed scopes")
		return
	}

	writeJSON(w, http.StatusOK, node)
}

// NodeHeartbeat handles POST /nodes/heartbeat
func (h *Handlers) NodeHeartbeat(w http.ResponseWriter, r *http.Request) {
	node := GetNodeFromContext(r.Context())
	if node == nil {
		writeError(w, http.StatusUnauthorized, "Invalid node API key")
		return
	}

	var heartbeat models.NodeHeartbeat
	if err := readJSON(r, &heartbeat); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if !heartbeat.Status.IsValid() {
		writeError(w, http.StatusBadRequest, "Invalid node status")
		return
	}

	updated, err := h.storage.UpdateNodeHeartbeat(node.ID, heartbeat.Status, heartbeat.CurrentTaskID)
	if err != nil {
		// Distinguish "node gone" from a transient DB error: a node that treats
		// every 404 as deregistration will re-register and needlessly rotate its
		// key, so transient failures must surface as 500 instead.
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "Node not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "Failed to update heartbeat")
		return
	}
	if updated == nil {
		writeError(w, http.StatusNotFound, "Node not found")
		return
	}

	writeJSON(w, http.StatusOK, updated)
}

// UnregisterNode handles DELETE /nodes/{node_id}
func (h *Handlers) UnregisterNode(w http.ResponseWriter, r *http.Request) {
	nodeIDStr := chi.URLParam(r, "node_id")
	nodeID, err := uuid.Parse(nodeIDStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid node ID")
		return
	}

	deleted, err := h.storage.RemoveNode(nodeID)
	if err != nil || !deleted {
		writeError(w, http.StatusNotFound, "Node not found")
		return
	}

	log.Printf("Node unregistered: %s", nodeID) //nolint:gosec // G706 false positive: nodeID is a uuid.UUID parsed via uuid.Parse, so its String() is canonical hex+dashes and cannot carry CR/LF.
	writeJSON(w, http.StatusOK, models.DeleteNodeResponse{
		Status: "deleted",
		NodeID: nodeIDStr,
	})
}

// Task Management Endpoints

// CreateTask handles POST /tasks
func (h *Handlers) CreateTask(w http.ResponseWriter, r *http.Request) {
	// Check for Admin API Key first
	isAdmin := h.verifyAdminKey(r)

	var creatorID *uuid.UUID
	// creatorKeyID is the scoped API key that authorized this task (if any), used
	// to attribute completion cost back to the key for spending caps.
	var creatorKeyID *string
	// creatorKeyMaxPriority is the authorizing key's task-urgency ceiling (#230),
	// copied by value so a later cap check can't be affected by the cached key
	// being mutated. nil = admin/user submission or an uncapped key.
	var creatorKeyMaxPriority *int

	if !isAdmin {
		// Check for scoped API key
		apiKey := r.Header.Get("X-API-Key")
		if apiKey != "" {
			perm := models.PermissionCreateTask
			valid, key, _ := h.apiKeys.ValidateKey(apiKey, &perm, nil, nil, nil)
			if valid && key != nil {
				isAdmin = true // Treat as authorized
				keyID := key.KeyID
				creatorKeyID = &keyID
				if key.MaxPriority != nil {
					capVal := *key.MaxPriority
					creatorKeyMaxPriority = &capVal
				}
			}
		}

		// Check for a User Token (password login) or the Elcano cookie.
		if !isAdmin {
			var user *models.User

			if authHeader := r.Header.Get("Authorization"); authHeader != "" {
				token := strings.TrimPrefix(authHeader, "Bearer ")
				if u, err := h.storage.GetUserByToken(token); err == nil && u != nil {
					user = u
				}
			}

			// Elcano unified-auth cookie (scoped tier): verify natively, then
			// require the email to be a provisioned user.
			if user == nil {
				if sess := h.elcanoSessionFromRequest(r); sess != nil {
					u, err := h.lookupMember(r.Context(), sess.Email)
					if err != nil && !errors.Is(err, sql.ErrNoRows) {
						writeError(w, http.StatusInternalServerError, "Membership check failed")
						return
					}
					if u == nil {
						writeJSON(w, http.StatusForbidden, map[string]string{"error": "not_a_member"})
						return
					}
					user = u
				}
			}

			if user == nil {
				writeError(w, http.StatusUnauthorized, "Unauthorized")
				return
			}

			creatorID = &user.ID
		}
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

	// Spending-cap pre-flight: refuse a key that has already reached its daily or
	// monthly LLM budget. Task cost is only known after completion, so this gates
	// on the already-accumulated spend.
	if creatorKeyID != nil {
		if err := h.apiKeys.CheckBudget(*creatorKeyID); err != nil {
			w.Header().Set("Retry-After", "3600")
			writeError(w, http.StatusTooManyRequests, err.Error())
			return
		}
	}

	task := models.NewTask(tc)
	task.CreatedBy = creatorID
	task.CreatedByKeyID = creatorKeyID

	// Per-key priority ceiling (#230): a scoped key capped at max_priority may not
	// submit a task MORE urgent (lower integer) than that. task.Priority is the
	// post-default value (0→Normal), so the comparison reflects what would run.
	// Shares priorityCapError with the batch path so the two can't drift.
	if err := priorityCapError(creatorKeyMaxPriority, task.Priority); err != nil {
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
	// Priority is bounded to [0,100] (#230); lower = more urgent. 0 is the unset
	// sentinel that NewTask maps to Normal (50), so it is accepted here.
	if tc.Priority < models.PriorityMin || tc.Priority > models.PriorityMax {
		return fmt.Errorf("priority must be between %d and %d", models.PriorityMin, models.PriorityMax)
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
	// personas/<name>.yaml. An unknown-but-safe name falls back to the global
	// persona at dispatch.
	if persona := strings.TrimSpace(tc.Persona); persona != "" {
		if strings.ContainsAny(persona, `/\`) || strings.Contains(persona, "..") || filepath.Base(persona) != persona {
			return fmt.Errorf("persona must be a bundle persona name without a path (got %q)", persona)
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

func (h *Handlers) validateTaskCreate(tc *models.TaskCreate) error {
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
	if err := validateTaskLimits(tc); err != nil {
		return err
	}
	if err := h.validateSandboxLimits(tc.SandboxLimits); err != nil {
		return err
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

	// If the principal is scoped (user or API key), add visibility filters.
	if scopes := p.scopes(); len(scopes) > 0 {
		filter.VisibleToUserID = p.ownerID()
		filter.VisibleToScopes = scopes
		// Ensure we use the filtered path
		hasFilters = true
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

// GetPendingTask handles GET /tasks/pending. Runners are retired — the
// in-process worker pool claims tasks directly via the runner package — but the
// endpoint remains so a node API key can claim the next pending task. It uses
// ClaimNextPendingTask (FOR UPDATE SKIP LOCKED) keyed on the requesting node as
// the lease owner, then builds the TaskAssignment carrying the per-task
// mcp_selection (no node targeting).
func (h *Handlers) GetPendingTask(w http.ResponseWriter, r *http.Request) {
	node := GetNodeFromContext(r.Context())
	if node == nil {
		writeError(w, http.StatusUnauthorized, "Invalid node API key")
		return
	}

	assignedTask, err := h.storage.ClaimNextPendingTask(r.Context(), node.ID.String())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to get pending task")
		return
	}
	if assignedTask == nil {
		writeJSON(w, http.StatusOK, nil)
		return
	}

	log.Printf("Task %s claimed by node %s (%s)", assignedTask.ID, node.Name, node.ID)

	// Build file checksums if files are present
	var fileChecksums []string
	if len(assignedTask.Files) > 0 {
		fileChecksums = h.getFileChecksums(assignedTask.Files)
	}

	writeJSON(w, http.StatusOK, models.TaskAssignment{
		TaskID:                 assignedTask.ID,
		Prompt:                 assignedTask.Prompt,
		Model:                  assignedTask.Model,
		FallbackModel:          assignedTask.FallbackModel,
		MaxIterations:          assignedTask.MaxIterations,
		MCPSelection:           assignedTask.MCPSelection,
		CredentialAllowlist:    assignedTask.CredentialAllowlist,
		InstructionSelfImprove: assignedTask.InstructionSelfImprove,
		OrchestratorURL:        h.config.OrchestratorURL,
		Files:                  assignedTask.Files,
		FileChecksums:          fileChecksums,
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

	if scopes := p.scopes(); len(scopes) > 0 {
		if !taskVisibleToScopes(task, scopes, p.ownerID()) {
			writeError(w, http.StatusForbidden, "Task not within allowed scopes")
			return
		}
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
	fallback := req.FallbackModel
	if fallback != "" {
		fp := &fallback
		if err := normalizeOptionalModel(&fp, "fallback_model"); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if fp != nil {
			fallback = *fp
		} else {
			fallback = ""
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
	log.Printf("Bulk re-assigned model=%q fallback=%q from=%q on %d scheduled task(s)", req.Model, fallback, req.FromModel, updated)
	writeJSON(w, http.StatusOK, models.BulkModelUpdateResult{DryRun: false, UpdatedCount: updated})
}

// CancelTask handles DELETE /tasks/{task_id}
func (h *Handlers) CancelTask(w http.ResponseWriter, r *http.Request) {
	p := h.principalFromRequest(r)
	if !p.hasPermission(models.PermissionCancelTask) {
		writeError(w, http.StatusForbidden, "Insufficient permissions")
		return
	}

	taskIDStr := chi.URLParam(r, "task_id")
	taskID, err := uuid.Parse(taskIDStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid task ID")
		return
	}

	// If the principal is scoped, verify it has access to this task.
	if scopes := p.scopes(); len(scopes) > 0 {
		task, err := h.storage.GetTask(taskID)
		if err != nil || task == nil {
			writeError(w, http.StatusNotFound, "Task not found")
			return
		}

		if !taskVisibleToScopes(task, scopes, p.ownerID()) {
			writeError(w, http.StatusForbidden, "Task not within allowed scopes")
			return
		}
	}

	// Use atomic cancel to prevent race conditions
	task, err := h.storage.CancelTaskAtomic(taskID)
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

	log.Printf("Task cancelled: %s", taskID) //nolint:gosec // G706 false positive: taskID is a uuid.UUID parsed via uuid.Parse; String() is canonical and cannot carry CR/LF.
	writeJSON(w, http.StatusOK, task)
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

	// If the principal is scoped, verify access.
	if scopes := p.scopes(); len(scopes) > 0 {
		if !taskVisibleToScopes(task, scopes, p.ownerID()) {
			writeError(w, http.StatusForbidden, "Task not within allowed scopes")
			return
		}
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
		AllowDelegation:        tc.AllowDelegation,
		Persona:                tc.Persona,
		ScheduledFor:           tc.ScheduledFor,
		Recurrence:             tc.Recurrence,
		Timezone:               tc.Timezone,
		Files:                  tc.Files,
		SetFiles:               tc.Files != nil,
		Tags:                   tc.Tags,
		SetTags:                tc.Tags != nil,
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

// GetSLAReport handles GET /admin/sla-report (#274): the per-prompt SLA
// actuals (p50/p95 actual duration + breach rate) over a window. The window is
// optional via ?days= (default 7, clamped to [1, 90] by the storage layer).
// Admin-gated (registered behind AdminAuthMiddleware) — duration/breach data
// is operator-sensitive, not public.
func (h *Handlers) GetSLAReport(w http.ResponseWriter, r *http.Request) {
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
	if scopes := p.scopes(); len(scopes) > 0 {
		if !taskVisibleToScopes(task, scopes, p.ownerID()) {
			writeError(w, http.StatusForbidden, "Task not within allowed scopes")
			return
		}
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
	Prompt          *string  `json:"prompt,omitempty"`
	Model           *string  `json:"model,omitempty"`
	FallbackModel   *string  `json:"fallback_model,omitempty"`
	MaxIterations   *int     `json:"max_iterations,omitempty"`
	Priority        *int     `json:"priority,omitempty"`
	AllowNetwork    *bool    `json:"allow_network,omitempty"`
	AllowDelegation *bool    `json:"allow_delegation,omitempty"`
	Description     *string  `json:"description,omitempty"`
	Tags            []string `json:"tags,omitempty"`
	Persona         *string  `json:"persona,omitempty"`
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
	if scopes := p.scopes(); len(scopes) > 0 {
		if !taskVisibleToScopes(source, scopes, p.ownerID()) {
			writeError(w, http.StatusForbidden, "Task not within allowed scopes")
			return
		}
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
// check, which re-samples a strictly-later now.
func buildRerunTaskCreate(source *models.Task, keepRecurrence bool, o taskRerunOverrides, fallbackLoc *time.Location) (models.TaskCreate, error) {
	tc := models.TaskToCreate(source)
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
		tc.AllowDelegation = *o.AllowDelegation
	}
	if o.Description != nil {
		tc.Description = *o.Description
	}
	if o.Tags != nil {
		tc.Tags = o.Tags
	}
	if o.Persona != nil {
		tc.Persona = *o.Persona
	}
}

// Status Reporting Endpoints

// ReportStatus handles POST /status
func (h *Handlers) ReportStatus(w http.ResponseWriter, r *http.Request) {
	node := GetNodeFromContext(r.Context())
	if node == nil {
		writeError(w, http.StatusUnauthorized, "Invalid node API key")
		return
	}

	var update models.StatusUpdate
	if err := readJSON(r, &update); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	// A node may only report progress/terminal statuses for its own task. It
	// must not be able to write orchestrator-owned states (e.g. "pending"),
	// which would let it re-queue and re-run a finished task.
	if !update.Status.IsValidReportedStatus() {
		writeError(w, http.StatusBadRequest, "Invalid task status")
		return
	}

	message := ""
	if update.Message != nil {
		message = " - " + *update.Message
	}
	log.Printf("Status update for task %s from node %s: %s%s", update.TaskID, node.Name, update.Status, message)

	// Use atomic update to prevent race conditions
	task, err := h.storage.UpdateTaskStatusAtomic(update.TaskID, node.ID, &update)
	if err != nil {
		if strings.Contains(err.Error(), "not assigned") {
			writeError(w, http.StatusForbidden, "Node is not assigned to this task")
			return
		}
		if strings.Contains(err.Error(), "no rows") {
			writeError(w, http.StatusNotFound, "Task not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "Failed to update task status")
		return
	}

	writeJSON(w, http.StatusOK, task)
}

// Log Submission Endpoints

// SubmitLogs handles POST /logs
func (h *Handlers) SubmitLogs(w http.ResponseWriter, r *http.Request) {
	node := GetNodeFromContext(r.Context())
	if node == nil {
		writeError(w, http.StatusUnauthorized, "Invalid node API key")
		return
	}

	// Limit request body size
	r.Body = http.MaxBytesReader(w, r.Body, models.MaxLogSubmissionSize)

	// Read the body to check size
	body, err := io.ReadAll(r.Body)
	if err != nil {
		if strings.Contains(err.Error(), "request body too large") {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("Log submission exceeds maximum size of %d bytes", models.MaxLogSubmissionSize))
			return
		}
		writeError(w, http.StatusBadRequest, "Failed to read request body")
		return
	}

	var submission models.LogSubmission
	if err := json.Unmarshal(body, &submission); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	task, err := h.storage.GetTask(submission.TaskID)
	if err != nil || task == nil {
		writeError(w, http.StatusNotFound, "Task not found")
		return
	}

	// Verify the node owns this task, by current assignment or by lease. A nil
	// assignment (e.g. after lease recovery) must NOT be treated as "anyone may
	// write" — otherwise any authenticated node could overwrite another task's
	// logs.
	ownsByAssignment := task.AssignedNodeID != nil && *task.AssignedNodeID == node.ID
	ownsByLease := task.LeaseOwner != nil && *task.LeaseOwner == node.ID.String()
	if !ownsByAssignment && !ownsByLease {
		writeError(w, http.StatusForbidden, "Node is not assigned to this task")
		return
	}

	log.Printf("Received logs for task %s from node %s: session %s with %d messages",
		task.ID, node.Name, submission.Session.ID, len(submission.Session.Messages))

	// Store the session logs
	if _, err := h.storage.AddLog(submission.TaskID, &submission.Session); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to store logs")
		return
	}

	// Update task with agent session ID if not already set
	if task.AgentSessionID == nil {
		task.AgentSessionID = &submission.Session.ID
		if _, err := h.storage.UpdateTask(task); err != nil {
			log.Printf("Warning: failed to update task with session ID: %v", err)
		}
	}

	// Attribute this run's LLM cost to the submitting API key (if any) for
	// per-key spending caps.
	if task.CreatedByKeyID != nil {
		h.apiKeys.AccumulateCost(*task.CreatedByKeyID, submission.Session.Cost)
	}

	writeJSON(w, http.StatusOK, submission.Session)
}

// GetLogs handles GET /logs/{task_id}
func (h *Handlers) GetLogs(w http.ResponseWriter, r *http.Request) {
	p := h.principalFromRequest(r)
	if !p.hasPermission(models.PermissionViewLogs) {
		writeError(w, http.StatusForbidden, "Insufficient permissions")
		return
	}

	taskIDStr := chi.URLParam(r, "task_id")
	taskID, err := uuid.Parse(taskIDStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid task ID")
		return
	}

	if scopes := p.scopes(); len(scopes) > 0 {
		task, err := h.storage.GetTask(taskID)
		if err != nil || task == nil {
			writeError(w, http.StatusNotFound, "Logs not found for this task")
			return
		}

		if !taskVisibleToScopes(task, scopes, p.ownerID()) {
			writeError(w, http.StatusForbidden, "Task not within allowed scopes")
			return
		}
	}

	session, err := h.storage.GetLog(taskID)
	if err != nil || session == nil {
		writeError(w, http.StatusNotFound, "Logs not found for this task")
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

	// Validate the optional task-urgency ceiling (#230) BEFORE creating the key,
	// so a bad value never leaves a half-created (uncapped) key behind.
	if keyCreate.MaxPriority != nil && (*keyCreate.MaxPriority < models.PriorityMin || *keyCreate.MaxPriority > models.PriorityMax) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("max_priority must be between %d and %d", models.PriorityMin, models.PriorityMax))
		return
	}

	key, rawKey, err := h.apiKeys.CreateKey(
		keyCreate.Name,
		keyCreate.AllowedNodePatterns,
		nil,
		keyCreate.Role,
		keyCreate.RateLimit,
		keyCreate.ExpiresInDays,
		keyCreate.Description,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to create API key")
		return
	}

	// Apply optional spending caps (CreateKey keeps a stable signature; budgets
	// are set in a follow-up so the field set can grow without churning callers).
	if keyCreate.MaxCostPerDayUSD != nil || keyCreate.MaxCostPerMonthUSD != nil {
		if err := h.apiKeys.SetBudgets(key.KeyID, keyCreate.MaxCostPerDayUSD, keyCreate.MaxCostPerMonthUSD); err != nil {
			log.Printf("Warning: failed to set budgets on new key %s: %v", key.KeyID, err)
		}
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

// GetKeySpending handles GET /keys/{key_id}/spending — current spend vs caps
// with next reset instants.
func (h *Handlers) GetKeySpending(w http.ResponseWriter, r *http.Request) {
	keyID := chi.URLParam(r, "key_id")
	snap, ok := h.apiKeys.SpendingSnapshot(keyID)
	if !ok {
		writeError(w, http.StatusNotFound, "API key not found")
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// ResetKeySpending handles POST /keys/{key_id}/reset-spending — operator
// override that zeros a key's accumulators (admin-gated by the route group).
func (h *Handlers) ResetKeySpending(w http.ResponseWriter, r *http.Request) {
	keyID := chi.URLParam(r, "key_id")
	if err := h.apiKeys.ResetSpending(keyID); err != nil {
		writeError(w, http.StatusNotFound, "API key not found")
		return
	}
	h.apiKeys.LogAction(keyID, "reset_spending", "api_key", &keyID, nil, nil, nil, true, nil)
	snap, _ := h.apiKeys.SpendingSnapshot(keyID)
	writeJSON(w, http.StatusOK, snap)
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

	var stats *models.DashboardStats
	var err error
	var scopes []string
	var scopeOwnerID *uuid.UUID

	// Check for scopes from user token
	if user != nil && len(user.Scopes) > 0 {
		scopes = user.Scopes
		scopeOwnerID = &user.ID
	}

	// Check for scopes from API key (if not already scoped by user)
	if len(scopes) == 0 && user == nil {
		// First check if API key was set in context by middleware
		if apiKeyFromCtx := GetAPIKeyFromContext(r.Context()); apiKeyFromCtx != nil {
			if len(apiKeyFromCtx.AllowedNodePatterns) > 0 {
				scopes = apiKeyFromCtx.AllowedNodePatterns
			}
		} else {
			// Fallback: check header directly. Skip the admin key (unrestricted)
			// using the constant-time verifier rather than a plain string compare.
			apiKey := r.Header.Get("X-API-Key")
			if apiKey != "" && !h.verifyAdminKey(r) {
				// Check if it's a scoped API key
				perm := models.PermissionViewTasks
				valid, key, _ := h.apiKeys.ValidateKey(apiKey, &perm, nil, nil, nil)
				if valid && key != nil && len(key.AllowedNodePatterns) > 0 {
					scopes = key.AllowedNodePatterns
				}
			}
		}
	}

	// Generate cache key
	cacheKey := "global"
	if len(scopes) > 0 {
		// Include scopes in key to ensure uniqueness even if user's scopes change
		// and to handle the API key case uniformly.
		scopesStr := strings.Join(scopes, ",")
		if scopeOwnerID != nil {
			cacheKey = "user:" + scopeOwnerID.String() + ":scopes:" + scopesStr
		} else {
			// For API keys, use the scopes as the key
			cacheKey = "scopes:" + scopesStr
		}
	}

	// Check cache (1 minute TTL)
	if cached, ok := h.statsCache.Get(cacheKey); ok {
		writeJSON(w, http.StatusOK, cached)
		return
	}

	// Get stats based on scopes
	if len(scopes) > 0 {
		stats, err = h.storage.GetDashboardStatsForUser(scopeOwnerID, scopes)
	} else {
		stats, err = h.storage.GetDashboardStats()
	}

	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to get dashboard stats")
		return
	}

	// Update cache
	h.statsCache.Set(cacheKey, stats, 1*time.Minute)

	writeJSON(w, http.StatusOK, stats)
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

// GetDashboardConfig handles GET /api/config
func (h *Handlers) GetDashboardConfig(w http.ResponseWriter, _ *http.Request) {
	config := map[string]interface{}{
		"version":  h.config.Version,
		"timezone": h.config.Timezone,
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

// getFileChecksums returns checksums for the given filenames.
func (h *Handlers) getFileChecksums(filenames []string) []string {
	if len(filenames) == 0 {
		return nil
	}

	checksums := make([]string, len(filenames))

	// Find the unique filenames and group their indices
	uniqueFiles := make(map[string][]int)
	for i, filename := range filenames {
		uniqueFiles[filename] = append(uniqueFiles[filename], i)
	}

	var wg sync.WaitGroup
	// Limit concurrency to avoid too many open files
	sem := make(chan struct{}, 20)

	for filename, indices := range uniqueFiles {
		// Check cache first
		if cached, ok := h.checksumCache.Get(filename); ok {
			for _, idx := range indices {
				checksums[idx] = cached
			}
			continue
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(fname string, idxs []int) {
			defer safe.Recover("sched.handlers.checksum", nil)
			defer wg.Done()
			defer func() { <-sem }()

			checksum, err := getFileChecksum(h.config.DataDir, fname)
			if err == nil {
				// Write directly to index - no lock needed since slices don't overlap
				for _, idx := range idxs {
					checksums[idx] = checksum
				}
				// Cache the result
				h.checksumCache.Set(fname, checksum)
			}
		}(filename, indices)
	}
	wg.Wait()

	return checksums
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
// enforce permissions and node-name scopes uniformly. Before this existed,
// scope and permission checks were gated on `user != nil`, which silently
// granted scoped API keys unrestricted, cross-tenant access (and let read-only
// keys mutate tasks).
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

// scopes returns the node-name glob patterns that constrain this principal's
// visibility. A nil/empty result means unrestricted access (admin key, or a
// user/key configured without scopes).
func (p principal) scopes() []string {
	if p.isAdmin {
		return nil
	}
	if p.user != nil {
		return p.user.Scopes
	}
	if p.apiKey != nil {
		return p.apiKey.AllowedNodePatterns
	}
	return nil
}

// ownerID returns the user ID used for creator-based task visibility, or nil
// for API-key principals (key-created tasks have no creator user).
func (p principal) ownerID() *uuid.UUID {
	if p.user != nil {
		return &p.user.ID
	}
	return nil
}

// scopesMatchNode reports whether any of the principal's scope patterns matches
// the given node name. An empty scope list is unrestricted. Nodes still carry
// names, so this gates node visibility in ListNodes/GetNode.
func scopesMatchNode(scopes []string, nodeName string) bool {
	if len(scopes) == 0 {
		return true
	}
	for _, scope := range scopes {
		if storage.MatchGlob(scope, nodeName) {
			return true
		}
	}
	return false
}

// taskVisibleToUser reports whether a task is visible to the given user under
// the user's node-name scopes.
func taskVisibleToUser(task *models.Task, user *models.User) bool {
	if user == nil {
		return false
	}
	return taskVisibleToScopes(task, user.Scopes, &user.ID)
}

// taskVisibleToScopes reports whether a task is visible to a principal
// constrained to the given node-name scope patterns. Tasks no longer carry a
// node target (the per-task mcp_selection replaced node routing), so a task is
// either: visible because the principal is unscoped, visible because it is the
// principal's own task, or — for any scoped principal — visible because an
// untargeted task can run anywhere. The result is therefore always true: every
// task is visible to every principal that reaches this check. ownerID (nil for
// API keys) is retained for signature compatibility with the unscoped fast path.
func taskVisibleToScopes(task *models.Task, scopes []string, ownerID *uuid.UUID) bool {
	if len(scopes) == 0 {
		return true
	}
	if ownerID != nil && task.CreatedBy != nil && *task.CreatedBy == *ownerID {
		return true
	}
	// Untargeted tasks can run on any node — including ones within the scope —
	// so every scoped principal may see and use them.
	return true
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
