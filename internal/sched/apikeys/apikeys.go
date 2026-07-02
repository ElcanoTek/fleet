// Package apikeys provides scoped API key management for the fleet orchestrator
// (sched). Ported from moc's internal/apikeys. The only change vs moc:
// CanTargetNode matches scope patterns via storage.MatchGlob directly (moc's
// NodeMatchesTask glob router was removed with per-task node routing).
package apikeys

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// APIKey represents a scoped API key.
type APIKey struct {
	KeyID               string              `json:"key_id"`
	Name                string              `json:"name"`
	KeyHash             string              `json:"key_hash"`
	KeyPrefix           string              `json:"key_prefix"`
	AllowedNodePatterns []string            `json:"allowed_node_patterns"`
	Permissions         []models.Permission `json:"permissions"`

	// Type is the key's access class (#190), encoded in its prefix
	// (fleet_{type}_…). Empty (KeyTypeLegacy) for untyped sk- keys minted before
	// typed keys existed — those are NOT subject to the type-scope gate.
	Type KeyType `json:"type,omitempty"`
	// AllowedTriggerSlugs scopes a webhook-type key to specific trigger slugs; it
	// is meaningless (and empty) for the other types.
	AllowedTriggerSlugs []string   `json:"allowed_trigger_slugs,omitempty"`
	RateLimit           int        `json:"rate_limit"`
	CreatedAt           time.Time  `json:"created_at"`
	RotatedAt           *time.Time `json:"rotated_at,omitempty"`
	ExpiresAt           *time.Time `json:"expires_at,omitempty"`
	Enabled             bool       `json:"enabled"`
	Description         string     `json:"description"`
	PreviousKeyHash     *string    `json:"previous_key_hash,omitempty"`
	PreviousKeyExpires  *time.Time `json:"previous_key_expires,omitempty"`

	// Spending caps (nil = unlimited). Cost is accumulated from the LogSession of
	// every task this key submitted; CheckBudget refuses new submissions once a
	// cap is reached. Counters reset lazily at the UTC day/month boundary on the
	// next access (no background goroutine).
	MaxCostPerDayUSD   *float64  `json:"max_cost_per_day_usd,omitempty"`
	MaxCostPerMonthUSD *float64  `json:"max_cost_per_month_usd,omitempty"`
	CostTodayUSD       float64   `json:"cost_today_usd"`
	CostThisMonthUSD   float64   `json:"cost_this_month_usd"`
	CostDayResetAt     time.Time `json:"cost_day_reset_at"`   // UTC day the daily counter is current for
	CostMonthResetAt   time.Time `json:"cost_month_reset_at"` // first of the UTC month the monthly counter is current for

	// MaxPriority caps the task urgency this key may submit (#230): a task at a
	// priority MORE urgent (lower integer) than this value is rejected. nil =
	// uncapped. Range [models.PriorityMin, models.PriorityMax].
	MaxPriority *int `json:"max_priority,omitempty"`
}

// CanTargetNode checks if this key can target a node with the given name.
func (k *APIKey) CanTargetNode(nodeName string) bool {
	if k.hasPermission(models.PermissionAdmin) {
		return true
	}
	if len(k.AllowedNodePatterns) == 0 {
		return true
	}
	for _, pattern := range k.AllowedNodePatterns {
		if storage.MatchGlob(pattern, nodeName) {
			return true
		}
	}
	return false
}

// AllowsTriggerSlug reports whether this (webhook) key is scoped to fire the
// given trigger slug. A key whose AllowedTriggerSlugs is empty permits no slug
// — a webhook key must be explicitly scoped to be useful.
func (k *APIKey) AllowsTriggerSlug(slug string) bool {
	for _, s := range k.AllowedTriggerSlugs {
		if s == slug {
			return true
		}
	}
	return false
}

// HasPermission checks if this key has a specific permission.
func (k *APIKey) HasPermission(perm models.Permission) bool { return k.hasPermission(perm) }

func (k *APIKey) hasPermission(perm models.Permission) bool {
	for _, p := range k.Permissions {
		if p == models.PermissionAdmin || p == perm {
			return true
		}
	}
	return false
}

// IsValid checks if the key is currently valid.
func (k *APIKey) IsValid() bool {
	if !k.Enabled {
		return false
	}
	if k.ExpiresAt != nil && time.Now().UTC().After(*k.ExpiresAt) {
		return false
	}
	return true
}

// ToResponse converts APIKey to APIKeyResponse.
func (k *APIKey) ToResponse() models.APIKeyResponse {
	perms := make([]string, len(k.Permissions))
	for i, p := range k.Permissions {
		perms[i] = string(p)
	}
	return models.APIKeyResponse{
		KeyID:               k.KeyID,
		Name:                k.Name,
		KeyPrefix:           k.KeyPrefix,
		Type:                string(k.Type),
		AllowedTriggerSlugs: k.AllowedTriggerSlugs,
		AllowedNodePatterns: k.AllowedNodePatterns,
		Permissions:         perms,
		RateLimit:           k.RateLimit,
		CreatedAt:           k.CreatedAt,
		RotatedAt:           k.RotatedAt,
		ExpiresAt:           k.ExpiresAt,
		Enabled:             k.Enabled,
		Description:         k.Description,
		MaxCostPerDayUSD:    k.MaxCostPerDayUSD,
		MaxCostPerMonthUSD:  k.MaxCostPerMonthUSD,
		CostTodayUSD:        k.CostTodayUSD,
		CostThisMonthUSD:    k.CostThisMonthUSD,
		MaxPriority:         k.MaxPriority,
	}
}

// AuditLogEntry represents an audit log entry.
type AuditLogEntry struct {
	Timestamp    time.Time              `json:"timestamp"`
	KeyID        string                 `json:"key_id"`
	Action       string                 `json:"action"`
	ResourceType string                 `json:"resource_type"`
	ResourceID   *string                `json:"resource_id,omitempty"`
	Details      map[string]interface{} `json:"details,omitempty"`
	IPAddress    *string                `json:"ip_address,omitempty"`
	UserAgent    *string                `json:"user_agent,omitempty"`
	Success      bool                   `json:"success"`
	ErrorMessage *string                `json:"error_message,omitempty"`
}

// RateLimitState tracks rate limit state for a key.
type RateLimitState struct {
	RequestCount int
	WindowStart  time.Time
}

// ResetIfNeeded resets counter if window has passed.
func (r *RateLimitState) ResetIfNeeded(windowHours int) {
	windowDuration := time.Duration(windowHours) * time.Hour
	if time.Since(r.WindowStart) > windowDuration {
		r.RequestCount = 0
		r.WindowStart = time.Now().UTC()
	}
}

// Increment increments and returns current count.
func (r *RateLimitState) Increment() int {
	r.RequestCount++
	return r.RequestCount
}

// Manager manages API keys with JSON file storage.
type Manager struct {
	storagePath  string
	auditLogPath string
	keys         map[string]*APIKey
	keyHashIndex map[string]string
	rateLimits   map[string]*RateLimitState
	// loadedModTime is the key file's mtime as of the last (re)load — the
	// cheap staleness check behind refreshIfChangedLocked.
	loadedModTime time.Time
	mu            sync.RWMutex
}

// NewManager creates a new API key manager.
func NewManager(storagePath, auditLogPath string) (*Manager, error) {
	if auditLogPath == "" {
		auditLogPath = filepath.Join(filepath.Dir(storagePath), "audit_log.jsonl")
	}
	m := &Manager{
		storagePath:  storagePath,
		auditLogPath: auditLogPath,
		keys:         make(map[string]*APIKey),
		keyHashIndex: make(map[string]string),
		rateLimits:   make(map[string]*RateLimitState),
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) hashKey(rawKey string) string {
	hash := sha256.Sum256([]byte(rawKey))
	return hex.EncodeToString(hash[:])
}

func (m *Manager) generateKey() (rawKey, keyHash, keyPrefix string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", "", fmt.Errorf("failed to generate random bytes for API key: %w", err)
	}
	rawKey = "sk-" + base64.URLEncoding.EncodeToString(b)
	keyHash = m.hashKey(rawKey)
	keyPrefix = rawKey[:11]
	return rawKey, keyHash, keyPrefix, nil
}

// generateTypedKey mints a typed key in the fleet_{type}_{base58} format (#190).
// The full raw key (type segment included) is what gets hashed, so a key's type
// is bound to its hash and cannot be spoofed on the wire. keyPrefix is a legible
// display fingerprint (type + first chars of the suffix) — never the full key.
func (m *Manager) generateTypedKey(kt KeyType) (rawKey, keyHash, keyPrefix string, err error) {
	suffix, err := randomBase58(keySuffixLen)
	if err != nil {
		return "", "", "", err
	}
	rawKey = typedKeyPrefix + string(kt) + "_" + suffix
	keyHash = m.hashKey(rawKey)
	fp := suffix
	if len(fp) > 6 {
		fp = fp[:6]
	}
	keyPrefix = typedKeyPrefix + string(kt) + "_" + fp
	return rawKey, keyHash, keyPrefix, nil
}

func (m *Manager) generateKeyID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate random bytes for key ID: %w", err)
	}
	return "key_" + hex.EncodeToString(b), nil
}

func (m *Manager) load() error {
	if st, err := os.Stat(m.storagePath); err == nil {
		m.loadedModTime = st.ModTime()
	}
	data, err := os.ReadFile(m.storagePath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var fileData struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(data, &fileData); err != nil {
		return fmt.Errorf("failed to parse API keys file: %w", err)
	}
	for _, keyData := range fileData.Keys {
		var key APIKey
		if err := json.Unmarshal(keyData, &key); err != nil {
			continue
		}
		m.keys[key.KeyID] = &key
		m.keyHashIndex[key.KeyHash] = key.KeyID
		if key.PreviousKeyHash != nil {
			m.keyHashIndex[*key.PreviousKeyHash] = key.KeyID
		}
	}
	return nil
}

// refreshIfChangedLocked re-reads the key file when its mtime moved past what
// this Manager last loaded — the seam that makes a key minted by the CLI (a
// SEPARATE process appending to api_keys.json) usable without restarting the
// server. Called with m.mu held, only on a LOOKUP MISS, so the steady state
// (known key, or garbage key against an unchanged file) never touches the
// filesystem beyond one Stat. Existing in-memory keys are kept (their runtime
// rate/budget state lives on the structs); only unseen key IDs are added, so a
// reload can never resurrect a key RevokeKey just removed but hadn't yet
// persisted. Returns true when new keys appeared and a retry is worthwhile.
func (m *Manager) refreshIfChangedLocked() bool {
	st, err := os.Stat(m.storagePath)
	if err != nil || !st.ModTime().After(m.loadedModTime) {
		return false
	}
	m.loadedModTime = st.ModTime()
	data, err := os.ReadFile(m.storagePath)
	if err != nil {
		return false
	}
	var fileData struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(data, &fileData); err != nil {
		return false
	}
	added := false
	for _, keyData := range fileData.Keys {
		var key APIKey
		if err := json.Unmarshal(keyData, &key); err != nil {
			continue
		}
		if _, known := m.keys[key.KeyID]; known {
			continue
		}
		k := key
		m.keys[k.KeyID] = &k
		m.keyHashIndex[k.KeyHash] = k.KeyID
		if k.PreviousKeyHash != nil {
			m.keyHashIndex[*k.PreviousKeyHash] = k.KeyID
		}
		added = true
	}
	return added
}

func (m *Manager) save() error {
	dir := filepath.Dir(m.storagePath)
	// 0700: the API-keys store holds secret material; only the owner needs it.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	keys := make([]*APIKey, 0, len(m.keys))
	for _, key := range m.keys {
		keys = append(keys, key)
	}
	data, err := json.MarshalIndent(map[string]interface{}{
		"version":    1,
		"updated_at": time.Now().UTC().Format(time.RFC3339),
		"keys":       keys,
	}, "", "  ")
	if err != nil {
		return err
	}
	// Write to a sibling temp file then atomically rename, so a crash mid-write
	// can't leave a torn api_keys.json that fails to deserialize and wedges
	// startup (NewManager would refuse to load it).
	tmp := m.storagePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.storagePath)
}

func (m *Manager) logAudit(entry AuditLogEntry) {
	dir := filepath.Dir(m.auditLogPath)
	// 0700: the audit log can reveal key IDs and operations; keep it owner-only.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("apikeys: failed to create audit log dir %s: %v", dir, err)
		return
	}
	entry.Timestamp = time.Now().UTC()
	data, err := json.Marshal(entry)
	if err != nil {
		log.Printf("apikeys: failed to marshal audit entry: %v", err)
		return
	}
	f, err := os.OpenFile(m.auditLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		log.Printf("apikeys: failed to open audit log %s: %v", m.auditLogPath, err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		log.Printf("apikeys: failed to append audit entry: %v", err)
	}
}

// CreateKey creates a new API key.
func (m *Manager) CreateKey(name string, allowedNodePatterns []string, permissions []models.Permission, role *string, rateLimit int, expiresInDays *int, description string) (*APIKey, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	rawKey, keyHash, keyPrefix, err := m.generateKey()
	if err != nil {
		return nil, "", err
	}
	keyID, err := m.generateKeyID()
	if err != nil {
		return nil, "", err
	}

	var perms []models.Permission
	if role != nil {
		if rolePerms, ok := models.RolePermissions[*role]; ok {
			perms = rolePerms
		}
	}
	if perms == nil && permissions != nil {
		perms = permissions
	}
	if perms == nil {
		perms = models.RolePermissions["client"]
	}

	var expiresAt *time.Time
	if expiresInDays != nil && *expiresInDays > 0 {
		t := time.Now().UTC().AddDate(0, 0, *expiresInDays)
		expiresAt = &t
	}

	if allowedNodePatterns == nil {
		allowedNodePatterns = []string{}
	}

	key := &APIKey{
		KeyID:               keyID,
		Name:                name,
		KeyHash:             keyHash,
		KeyPrefix:           keyPrefix,
		AllowedNodePatterns: allowedNodePatterns,
		Permissions:         perms,
		RateLimit:           rateLimit,
		CreatedAt:           time.Now().UTC(),
		ExpiresAt:           expiresAt,
		Enabled:             true,
		Description:         description,
	}

	m.keys[keyID] = key
	m.keyHashIndex[keyHash] = keyID

	if err := m.save(); err != nil {
		delete(m.keys, keyID)
		delete(m.keyHashIndex, keyHash)
		return nil, "", err
	}

	permStrings := make([]string, len(perms))
	for i, p := range perms {
		permStrings[i] = string(p)
	}
	m.logAudit(AuditLogEntry{
		KeyID:        keyID,
		Action:       "create_key",
		ResourceType: "api_key",
		ResourceID:   &keyID,
		Details: map[string]interface{}{
			"name":                  name,
			"allowed_node_patterns": allowedNodePatterns,
			"permissions":           permStrings,
			"rate_limit":            rateLimit,
		},
		Success: true,
	})
	return key, rawKey, nil
}

// CreateTypedKey creates a new typed API key (#190), minting a
// fleet_{type}_{base58} token whose permission set is DERIVED from the type
// (KeyType.Permissions). allowedTriggerSlugs scopes a webhook key to specific
// trigger slugs and is ignored for the other types. The raw key is returned
// once; only its hash is stored.
func (m *Manager) CreateTypedKey(name string, kt KeyType, allowedTriggerSlugs, allowedNodePatterns []string, rateLimit int, expiresInDays *int, description string) (*APIKey, string, error) {
	if !kt.Valid() {
		return nil, "", fmt.Errorf("invalid key type: %q", kt)
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	rawKey, keyHash, keyPrefix, err := m.generateTypedKey(kt)
	if err != nil {
		return nil, "", err
	}
	keyID, err := m.generateKeyID()
	if err != nil {
		return nil, "", err
	}

	var expiresAt *time.Time
	if expiresInDays != nil && *expiresInDays > 0 {
		t := time.Now().UTC().AddDate(0, 0, *expiresInDays)
		expiresAt = &t
	}
	if allowedNodePatterns == nil {
		allowedNodePatterns = []string{}
	}
	// Trigger slugs are meaningful only for webhook keys; drop any supplied for
	// another type so the stored record can't carry misleading scope.
	if kt != KeyTypeWebhook {
		allowedTriggerSlugs = nil
	}

	perms := kt.Permissions()
	key := &APIKey{
		KeyID:               keyID,
		Name:                name,
		KeyHash:             keyHash,
		KeyPrefix:           keyPrefix,
		Type:                kt,
		AllowedTriggerSlugs: allowedTriggerSlugs,
		AllowedNodePatterns: allowedNodePatterns,
		Permissions:         perms,
		RateLimit:           rateLimit,
		CreatedAt:           time.Now().UTC(),
		ExpiresAt:           expiresAt,
		Enabled:             true,
		Description:         description,
	}

	m.keys[keyID] = key
	m.keyHashIndex[keyHash] = keyID

	if err := m.save(); err != nil {
		delete(m.keys, keyID)
		delete(m.keyHashIndex, keyHash)
		return nil, "", err
	}

	permStrings := make([]string, len(perms))
	for i, p := range perms {
		permStrings[i] = string(p)
	}
	m.logAudit(AuditLogEntry{
		KeyID:        keyID,
		Action:       "create_key",
		ResourceType: "api_key",
		ResourceID:   &keyID,
		Details: map[string]interface{}{
			"name":                  name,
			"type":                  string(kt),
			"allowed_trigger_slugs": allowedTriggerSlugs,
			"allowed_node_patterns": allowedNodePatterns,
			"permissions":           permStrings,
			"rate_limit":            rateLimit,
		},
		Success: true,
	})
	return key, rawKey, nil
}

// RotateKey rotates an API key, keeping the old key valid for a grace period.
func (m *Manager) RotateKey(keyID string, gracePeriodHours int) (*APIKey, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	key, ok := m.keys[keyID]
	if !ok {
		return nil, "", fmt.Errorf("key %s not found", keyID)
	}
	// Preserve the key's type across rotation (#190): a typed key rotates to a
	// fresh token of the SAME type, so its scope is unchanged; a legacy (sk-) key
	// stays legacy.
	var (
		rawKey, keyHash, keyPrefix string
		err                        error
	)
	if key.Type != KeyTypeLegacy {
		rawKey, keyHash, keyPrefix, err = m.generateTypedKey(key.Type)
	} else {
		rawKey, keyHash, keyPrefix, err = m.generateKey()
	}
	if err != nil {
		return nil, "", err
	}
	oldHash := key.KeyHash
	now := time.Now().UTC()
	graceExpires := now.Add(time.Duration(gracePeriodHours) * time.Hour)
	key.PreviousKeyHash = &oldHash
	key.PreviousKeyExpires = &graceExpires
	key.KeyHash = keyHash
	key.KeyPrefix = keyPrefix
	key.RotatedAt = &now
	m.keyHashIndex[keyHash] = keyID
	if err := m.save(); err != nil {
		return nil, "", err
	}
	m.logAudit(AuditLogEntry{
		KeyID:        keyID,
		Action:       "rotate_key",
		ResourceType: "api_key",
		ResourceID:   &keyID,
		Details:      map[string]interface{}{"grace_period_hours": gracePeriodHours, "new_prefix": keyPrefix},
		Success:      true,
	})
	return key, rawKey, nil
}

// RevokeKey revokes (disables) an API key.
func (m *Manager) RevokeKey(keyID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key, ok := m.keys[keyID]
	if !ok {
		return fmt.Errorf("key %s not found", keyID)
	}
	key.Enabled = false
	if err := m.save(); err != nil {
		return err
	}
	m.logAudit(AuditLogEntry{KeyID: keyID, Action: "revoke_key", ResourceType: "api_key", ResourceID: &keyID, Success: true})
	return nil
}

// DeleteKey permanently deletes an API key.
func (m *Manager) DeleteKey(keyID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key, ok := m.keys[keyID]
	if !ok {
		return fmt.Errorf("key %s not found", keyID)
	}
	delete(m.keyHashIndex, key.KeyHash)
	if key.PreviousKeyHash != nil {
		delete(m.keyHashIndex, *key.PreviousKeyHash)
	}
	delete(m.keys, keyID)
	if err := m.save(); err != nil {
		return err
	}
	m.logAudit(AuditLogEntry{KeyID: keyID, Action: "delete_key", ResourceType: "api_key", ResourceID: &keyID, Success: true})
	return nil
}

// ValidateKey validates an API key and checks permissions.
func (m *Manager) ValidateKey(rawKey string, requiredPermission *models.Permission, targetNodeName, ipAddress, userAgent *string) (bool, *APIKey, string) {
	keyHash := m.hashKey(rawKey)

	m.mu.Lock()
	defer m.mu.Unlock()

	keyID, ok := m.keyHashIndex[keyHash]
	if !ok && m.refreshIfChangedLocked() {
		keyID, ok = m.keyHashIndex[keyHash]
	}
	if !ok {
		return false, nil, "Invalid API key"
	}
	key, ok := m.keys[keyID]
	if !ok {
		return false, nil, "Invalid API key"
	}

	if key.PreviousKeyHash != nil && keyHash == *key.PreviousKeyHash {
		if key.PreviousKeyExpires != nil && time.Now().UTC().After(*key.PreviousKeyExpires) {
			delete(m.keyHashIndex, *key.PreviousKeyHash)
			key.PreviousKeyHash = nil
			key.PreviousKeyExpires = nil
			// Persist the retirement of the rotated-out previous key. If this
			// fails the in-memory index is already updated (the old hash will be
			// rejected this process), but the change must survive a restart, so
			// surface the failure rather than dropping it silently.
			if err := m.save(); err != nil {
				log.Printf("apikeys: failed to persist expiry of rotated key %s: %v", key.KeyID, err)
			}
			return false, nil, "API key has been rotated"
		}
	}

	if !key.IsValid() {
		var errMsg string
		if !key.Enabled {
			errMsg = "API key is disabled"
		} else {
			errMsg = "API key has expired"
		}
		m.logAudit(AuditLogEntry{KeyID: keyID, Action: "validate_key", ResourceType: "api_key", ResourceID: &keyID, IPAddress: ipAddress, UserAgent: userAgent, Success: false, ErrorMessage: &errMsg})
		return false, nil, errMsg
	}

	if key.RateLimit > 0 {
		state, exists := m.rateLimits[keyID]
		if !exists {
			state = &RateLimitState{WindowStart: time.Now().UTC()}
			m.rateLimits[keyID] = state
		}
		state.ResetIfNeeded(1)
		if state.RequestCount >= key.RateLimit {
			errMsg := fmt.Sprintf("Rate limit exceeded: %d/hour", key.RateLimit)
			m.logAudit(AuditLogEntry{KeyID: keyID, Action: "rate_limit_exceeded", ResourceType: "api_key", ResourceID: &keyID, IPAddress: ipAddress, UserAgent: userAgent, Success: false, ErrorMessage: &errMsg})
			return false, nil, fmt.Sprintf("Rate limit exceeded (%d requests/hour)", key.RateLimit)
		}
		state.Increment()
	}

	if requiredPermission != nil && !key.HasPermission(*requiredPermission) {
		errMsg := fmt.Sprintf("Missing permission: %s", *requiredPermission)
		m.logAudit(AuditLogEntry{KeyID: keyID, Action: "permission_denied", ResourceType: "api_key", ResourceID: &keyID, Details: map[string]interface{}{"required_permission": string(*requiredPermission)}, IPAddress: ipAddress, UserAgent: userAgent, Success: false, ErrorMessage: &errMsg})
		return false, nil, errMsg
	}

	if targetNodeName != nil && !key.CanTargetNode(*targetNodeName) {
		errMsg := fmt.Sprintf("Cannot target node: %s", *targetNodeName)
		m.logAudit(AuditLogEntry{KeyID: keyID, Action: "node_access_denied", ResourceType: "api_key", ResourceID: &keyID, Details: map[string]interface{}{"target_node": *targetNodeName}, IPAddress: ipAddress, UserAgent: userAgent, Success: false, ErrorMessage: &errMsg})
		return false, nil, errMsg
	}

	return true, key, ""
}

// LookupKeyMeta resolves a raw key to its stable key ID and per-key rate-limit
// override WITHOUT enforcing or mutating any rate-limit state — a read-only
// helper for the orchestrator's sliding-window task limiter (#247), which keys
// its window by the stable key ID and treats RateLimit (when > 0) as that key's
// per-minute cap. Returns ok=false for unknown, disabled, or expired keys.
func (m *Manager) LookupKeyMeta(rawKey string) (keyID string, rateLimit int, ok bool) {
	keyHash := m.hashKey(rawKey)
	m.mu.RLock()
	id, found := m.keyHashIndex[keyHash]
	if !found {
		// Miss: the key may have been minted by another process (the CLI) since
		// this Manager loaded. Upgrade to a write lock, refresh, retry once.
		m.mu.RUnlock()
		m.mu.Lock()
		if m.refreshIfChangedLocked() {
			id, found = m.keyHashIndex[keyHash]
		}
		m.mu.Unlock()
		m.mu.RLock()
	}
	defer m.mu.RUnlock()
	if !found {
		return "", 0, false
	}
	key, found := m.keys[id]
	if !found || !key.IsValid() {
		return "", 0, false
	}
	return key.KeyID, key.RateLimit, true
}

// LookupKeyType resolves a raw key to its access class and whether it carries
// task-create permission, WITHOUT enforcing or mutating any rate-limit state
// (#190) — a read-only helper for the task-create paths' under-scope gate.
// Returns ok=false for unknown, disabled, or expired keys. A legacy (untyped)
// key returns KeyTypeLegacy, so callers can preserve historical behavior for it.
func (m *Manager) LookupKeyType(rawKey string) (kt KeyType, hasCreate, ok bool) {
	keyHash := m.hashKey(rawKey)
	m.mu.RLock()
	id, found := m.keyHashIndex[keyHash]
	if !found {
		// Same CLI-minted-key refresh as LookupKeyMeta.
		m.mu.RUnlock()
		m.mu.Lock()
		if m.refreshIfChangedLocked() {
			id, found = m.keyHashIndex[keyHash]
		}
		m.mu.Unlock()
		m.mu.RLock()
	}
	defer m.mu.RUnlock()
	if !found {
		return "", false, false
	}
	key, found := m.keys[id]
	if !found || !key.IsValid() {
		return "", false, false
	}
	return key.Type, key.HasPermission(models.PermissionCreateTask), true
}

// ConsumeN charges a key's hourly rate-limit counter for n additional requests
// (#227). It is the multi-token counterpart to the single increment
// ValidateKey performs: the batch task endpoint must not be a rate-limit bypass,
// so a batch of N tasks consumes N tokens total. ValidateKey has already
// consumed 1 when it authorized the caller, so the handler passes n = (batch
// size - 1) to reach the full count. Returns false (without mutating state) when
// the remaining hourly budget cannot cover n, leaving the caller to surface a
// 429. A key with RateLimit == 0 (no per-key cap) is a no-op success. An
// unknown / already-authorized-via-admin path passes keyID = "" → no-op success
// (the admin key and cookie/bearer callers are not keyed here).
func (m *Manager) ConsumeN(keyID string, n int) bool {
	if keyID == "" || n <= 0 {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key, ok := m.keys[keyID]
	if !ok || key.RateLimit <= 0 {
		return true
	}
	state, exists := m.rateLimits[keyID]
	if !exists {
		state = &RateLimitState{WindowStart: time.Now().UTC()}
		m.rateLimits[keyID] = state
	}
	state.ResetIfNeeded(1)
	if state.RequestCount+n > key.RateLimit {
		return false
	}
	state.RequestCount += n
	return true
}

// SetBudgets sets (or clears, when nil) a key's daily/monthly spending caps and
// persists them. Used by the create/update handlers. Returns an error for an
// unknown key.
func (m *Manager) SetBudgets(keyID string, daily, monthly *float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key, ok := m.keys[keyID]
	if !ok {
		return fmt.Errorf("api key not found: %s", keyID)
	}
	key.MaxCostPerDayUSD = daily
	key.MaxCostPerMonthUSD = monthly
	return m.save()
}

// SetMaxPriority sets (or clears, when max is nil) a key's task-urgency ceiling
// (#230) and persists it. Used by the create/update handlers. Returns an error
// for an unknown key.
func (m *Manager) SetMaxPriority(keyID string, ceiling *int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key, ok := m.keys[keyID]
	if !ok {
		return fmt.Errorf("api key not found: %s", keyID)
	}
	key.MaxPriority = ceiling
	return m.save()
}

// maybeResetSpendingLocked zeros a key's daily/monthly accumulators when their
// UTC window has rolled over since the counter was last current. Lazy (no
// background goroutine): called under m.mu by AccumulateCost / CheckBudget /
// spending reads. Returns true when anything changed (so callers can persist).
func maybeResetSpendingLocked(key *APIKey, now time.Time) bool {
	changed := false
	today := now.UTC().Truncate(24 * time.Hour)
	if key.CostDayResetAt.Before(today) {
		key.CostTodayUSD = 0
		key.CostDayResetAt = today
		changed = true
	}
	monthStart := time.Date(now.UTC().Year(), now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	if key.CostMonthResetAt.Before(monthStart) {
		key.CostThisMonthUSD = 0
		key.CostMonthResetAt = monthStart
		changed = true
	}
	return changed
}

// AccumulateCost adds costUSD to a key's daily and monthly running totals
// (resetting first if a window rolled over) and persists. No-op for an unknown
// key or non-positive cost.
func (m *Manager) AccumulateCost(keyID string, costUSD float64) {
	if costUSD <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key, ok := m.keys[keyID]
	if !ok {
		return
	}
	maybeResetSpendingLocked(key, time.Now())
	key.CostTodayUSD += costUSD
	key.CostThisMonthUSD += costUSD
	if err := m.save(); err != nil {
		log.Printf("apikeys: failed to persist accumulated cost for key %s: %v", keyID, err)
	}
}

// CheckBudget reports whether keyID may submit more work. It refuses once the
// already-accumulated spend has reached a configured daily or monthly cap (task
// cost is only known after completion, so this is a hard "already over" gate).
// Returns nil for an unknown key (ValidateKey surfaces that) or one with no caps.
func (m *Manager) CheckBudget(keyID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key, ok := m.keys[keyID]
	if !ok {
		return nil
	}
	if maybeResetSpendingLocked(key, time.Now()) {
		if err := m.save(); err != nil {
			log.Printf("apikeys: failed to persist spending reset for key %s: %v", keyID, err)
		}
	}
	if key.MaxCostPerDayUSD != nil && key.CostTodayUSD >= *key.MaxCostPerDayUSD {
		return fmt.Errorf("daily budget exceeded: spent $%.4f of $%.2f daily cap", key.CostTodayUSD, *key.MaxCostPerDayUSD)
	}
	if key.MaxCostPerMonthUSD != nil && key.CostThisMonthUSD >= *key.MaxCostPerMonthUSD {
		return fmt.Errorf("monthly budget exceeded: spent $%.4f of $%.2f monthly cap", key.CostThisMonthUSD, *key.MaxCostPerMonthUSD)
	}
	return nil
}

// ResetSpending zeros a key's daily and monthly accumulators (operator override,
// e.g. after a billing dispute) and persists. Returns an error for an unknown key.
func (m *Manager) ResetSpending(keyID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key, ok := m.keys[keyID]
	if !ok {
		return fmt.Errorf("api key not found: %s", keyID)
	}
	now := time.Now().UTC()
	key.CostTodayUSD = 0
	key.CostThisMonthUSD = 0
	key.CostDayResetAt = now.Truncate(24 * time.Hour)
	key.CostMonthResetAt = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return m.save()
}

// SpendingSnapshot returns a key's current spend vs caps with the next reset
// instants, applying any pending lazy reset first. ok=false for an unknown key.
func (m *Manager) SpendingSnapshot(keyID string) (snap models.APIKeySpending, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key, found := m.keys[keyID]
	if !found {
		return models.APIKeySpending{}, false
	}
	if maybeResetSpendingLocked(key, time.Now()) {
		if err := m.save(); err != nil {
			log.Printf("apikeys: failed to persist spending reset for key %s: %v", keyID, err)
		}
	}
	now := time.Now().UTC()
	return models.APIKeySpending{
		KeyID:              key.KeyID,
		CostTodayUSD:       key.CostTodayUSD,
		MaxCostPerDayUSD:   key.MaxCostPerDayUSD,
		CostThisMonthUSD:   key.CostThisMonthUSD,
		MaxCostPerMonthUSD: key.MaxCostPerMonthUSD,
		DailyResetAt:       now.Truncate(24*time.Hour).AddDate(0, 0, 1),
		MonthlyResetAt:     time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0),
	}, true
}

// LogAction logs an action performed with an API key.
func (m *Manager) LogAction(keyID, action, resourceType string, resourceID *string, details map[string]interface{}, ipAddress, userAgent *string, success bool, errorMessage *string) {
	m.logAudit(AuditLogEntry{KeyID: keyID, Action: action, ResourceType: resourceType, ResourceID: resourceID, Details: details, IPAddress: ipAddress, UserAgent: userAgent, Success: success, ErrorMessage: errorMessage})
}

// GetKey gets a key by ID.
func (m *Manager) GetKey(keyID string) *APIKey {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.keys[keyID]
}

// ListKeys lists all API keys.
func (m *Manager) ListKeys() []*APIKey {
	m.mu.RLock()
	defer m.mu.RUnlock()
	keys := make([]*APIKey, 0, len(m.keys))
	for _, key := range m.keys {
		keys = append(keys, key)
	}
	return keys
}

// reverseLineScanner reads a file backwards line by line.
type reverseLineScanner struct {
	file *os.File
	pos  int64
	buf  []byte
}

func newReverseLineScanner(f *os.File) (*reverseLineScanner, error) {
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return &reverseLineScanner{file: f, pos: stat.Size(), buf: make([]byte, 0, 4096)}, nil
}

func (s *reverseLineScanner) scan() ([]byte, error) {
	if len(s.buf) > 0 {
		if idx := bytes.LastIndexByte(s.buf, '\n'); idx >= 0 {
			line := s.buf[idx+1:]
			s.buf = s.buf[:idx]
			if len(s.buf) > 0 && s.buf[len(s.buf)-1] == '\r' {
				s.buf = s.buf[:len(s.buf)-1]
			}
			return line, nil
		}
	}
	for s.pos > 0 {
		chunkSize := int64(4096)
		if s.pos < chunkSize {
			chunkSize = s.pos
		}
		s.pos -= chunkSize
		// Over-allocate capacity so the prepend below (append(chunk, s.buf...))
		// can reuse chunk's backing array instead of reallocating.
		chunk := make([]byte, chunkSize, chunkSize+int64(len(s.buf)))
		if _, err := s.file.ReadAt(chunk, s.pos); err != nil {
			return nil, err
		}
		s.buf = append(chunk, s.buf...)
		if idx := bytes.LastIndexByte(s.buf, '\n'); idx >= 0 {
			line := s.buf[idx+1:]
			s.buf = s.buf[:idx]
			if len(s.buf) > 0 && s.buf[len(s.buf)-1] == '\r' {
				s.buf = s.buf[:len(s.buf)-1]
			}
			return line, nil
		}
	}
	if len(s.buf) > 0 {
		line := s.buf
		s.buf = nil
		return line, nil
	}
	return nil, io.EOF
}

// GetAuditLog reads audit log entries (newest first).
func (m *Manager) GetAuditLog(keyID, action *string, since *time.Time, limit int) []map[string]interface{} {
	f, err := os.Open(m.auditLogPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return nil
	}
	defer f.Close()

	scanner, err := newReverseLineScanner(f)
	if err != nil {
		return nil
	}

	var entries []map[string]interface{}
	var keyIDSearch, actionSearch []byte
	if keyID != nil {
		b, _ := json.Marshal(map[string]string{"key_id": *keyID})
		if len(b) > 2 {
			keyIDSearch = b[1 : len(b)-1]
		}
	}
	if action != nil {
		b, _ := json.Marshal(map[string]string{"action": *action})
		if len(b) > 2 {
			actionSearch = b[1 : len(b)-1]
		}
	}

	for {
		line, err := scanner.scan()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			break
		}
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		if keyIDSearch != nil && !bytes.Contains(trimmed, keyIDSearch) {
			continue
		}
		if actionSearch != nil && !bytes.Contains(trimmed, actionSearch) {
			continue
		}
		var entry map[string]interface{}
		if err := json.Unmarshal(trimmed, &entry); err != nil {
			continue
		}
		match := true
		if keyID != nil && entry["key_id"] != *keyID {
			match = false
		}
		if match && action != nil && entry["action"] != *action {
			match = false
		}
		if match {
			if ts, ok := entry["timestamp"].(string); ok {
				entryTime, _ := time.Parse(time.RFC3339, ts)
				if since != nil && entryTime.Before(*since) {
					break
				}
			}
			entries = append(entries, entry)
			if limit > 0 && len(entries) >= limit {
				break
			}
		}
	}
	return entries
}

// Global manager instance
var globalManager *Manager

// GetManager returns the global API key manager.
func GetManager() *Manager { return globalManager }

// InitGlobalManager initializes the global API key manager.
func InitGlobalManager(dataDir string) error {
	var err error
	globalManager, err = NewManager(
		filepath.Join(dataDir, "api_keys.json"),
		filepath.Join(dataDir, "audit_log.jsonl"),
	)
	return err
}
