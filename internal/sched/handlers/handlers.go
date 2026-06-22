// Copyright (c) 2025 ElcanoTek
// All rights reserved. This is a private repository.

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

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/robfig/cron/v3"

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

	// Elcano unified auth (scoped tier). ElcanoPubKey is the Ed25519 public
	// key the server verifies the elcano_auth cookie with; nil disables the
	// cookie path (the button renders but every cookie fails closed). See
	// elcano.go.
	ElcanoPubKey       ed25519.PublicKey
	AuthLoginURL       string // e.g. https://auth.elcanotek.com (no trailing slash)
	ElcanoCookieName   string // default "elcano_auth"
	ElcanoCookieDomain string // AUTH_COOKIE_DOMAIN (e.g. "elcanotek.com"); "" = host-only. Needed to delete the shared cookie on logout.
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

	// Cache for file checksums to avoid repeated disk I/O
	checksumCache *checksumCache

	// Cache for dashboard stats
	statsCache *statsCache

	// memberLookup resolves an email to a user for the scoped-tier gate
	// (elcano_auth cookie path). nil in production → falls back to
	// storage.GetUserByUsername. Tests inject a fake to avoid a live database.
	memberLookup func(ctx context.Context, email string) (*models.User, error)
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
	taskPromptMinLength  = 3
	taskPromptMaxLength  = 100000
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

// New creates a new Handlers instance.
func New(cfg Config, store *storage.Storage, keyMgr *apikeys.Manager) *Handlers {
	return &Handlers{
		config:           cfg,
		storage:          store,
		apiKeys:          keyMgr,
		regRateLimiter:   newRateLimiter(10, time.Minute), // 10 registrations per minute per IP
		loginRateLimiter: newRateLimiter(20, time.Minute), // 20 logins per minute per IP
		checksumCache:    newChecksumCache(),
		statsCache:       newStatsCache(),
	}
}

// Helper functions

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, models.ErrorResponse{Detail: detail})
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

	log.Printf("Node unregistered: %s", nodeID)
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

	if !isAdmin {
		// Check for scoped API key
		apiKey := r.Header.Get("X-API-Key")
		if apiKey != "" {
			perm := models.PermissionCreateTask
			valid, key, _ := h.apiKeys.ValidateKey(apiKey, &perm, nil, nil, nil)
			if valid && key != nil {
				isAdmin = true // Treat as authorized
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

	if err := h.validateTaskCreate(&tc); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	task := models.NewTask(tc)
	task.CreatedBy = creatorID

	if _, err := h.storage.AddTask(task); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to create task")
		return
	}

	log.Printf("Task created: %s (prompt: %.50s...)", task.ID, task.Prompt)
	writeJSON(w, http.StatusOK, task)
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

	// Light validation of the per-task MCP selection: each chosen server must
	// be named. Account is optional (empty means the default/shared seat).
	for _, choice := range tc.MCPSelection {
		if strings.TrimSpace(choice.Server) == "" {
			return fmt.Errorf("mcp_selection entries must name a server")
		}
	}

	if err := normalizeOptionalModel(&tc.Model, "model"); err != nil {
		return err
	}
	if err := normalizeOptionalModel(&tc.FallbackModel, "fallback_model"); err != nil {
		return err
	}
	if tc.MaxIterations != nil {
		if *tc.MaxIterations < 1 || *tc.MaxIterations > 10000 {
			return fmt.Errorf("max_iterations must be between 1 and 10000")
		}
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
			if tc.ScheduledFor == nil {
				now := time.Now().In(h.storage.Location())
				next := schedule.Next(now)
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

	log.Printf("Task cancelled: %s", taskID)
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

			next := schedule.Next(time.Now().In(h.storage.Location()))
			nextUTC := next.UTC()
			tc.ScheduledFor = &nextUTC
		}
	}

	// Apply updates transactionally: the storage layer re-locks the row and
	// re-checks that it is still editable, so a node leasing the task or a
	// cancellation between our read above and this write cannot be silently
	// overwritten (which would resurrect the task and clobber its lease).
	edit := storage.TaskEdit{
		Prompt:                 tc.Prompt,
		Model:                  tc.Model,
		FallbackModel:          tc.FallbackModel,
		MaxIterations:          tc.MaxIterations,
		MCPSelection:           tc.MCPSelection,
		SetMCPSelection:        tc.MCPSelection != nil,
		Priority:               tc.Priority,
		InstructionSelfImprove: tc.InstructionSelfImprove,
		ScheduledFor:           tc.ScheduledFor,
		Recurrence:             tc.Recurrence,
		Files:                  tc.Files,
		SetFiles:               tc.Files != nil,
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

	log.Printf("Task updated: %s (prompt: %.50s...)", updated.ID, updated.Prompt)
	writeJSON(w, http.StatusOK, updated)
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

	log.Printf("Rotated API key: %s", keyID)

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

	log.Printf("Revoked API key: %s", keyID)

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

	log.Printf("Deleted API key: %s", keyID)
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
