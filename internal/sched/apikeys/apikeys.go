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
	RateLimit           int                 `json:"rate_limit"`
	CreatedAt           time.Time           `json:"created_at"`
	RotatedAt           *time.Time          `json:"rotated_at,omitempty"`
	ExpiresAt           *time.Time          `json:"expires_at,omitempty"`
	Enabled             bool                `json:"enabled"`
	Description         string              `json:"description"`
	PreviousKeyHash     *string             `json:"previous_key_hash,omitempty"`
	PreviousKeyExpires  *time.Time          `json:"previous_key_expires,omitempty"`
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
		AllowedNodePatterns: k.AllowedNodePatterns,
		Permissions:         perms,
		RateLimit:           k.RateLimit,
		CreatedAt:           k.CreatedAt,
		RotatedAt:           k.RotatedAt,
		ExpiresAt:           k.ExpiresAt,
		Enabled:             k.Enabled,
		Description:         k.Description,
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
	mu           sync.RWMutex
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

func (m *Manager) generateKeyID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate random bytes for key ID: %w", err)
	}
	return "key_" + hex.EncodeToString(b), nil
}

func (m *Manager) load() error {
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

// RotateKey rotates an API key, keeping the old key valid for a grace period.
func (m *Manager) RotateKey(keyID string, gracePeriodHours int) (*APIKey, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	key, ok := m.keys[keyID]
	if !ok {
		return nil, "", fmt.Errorf("key %s not found", keyID)
	}
	rawKey, keyHash, keyPrefix, err := m.generateKey()
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
