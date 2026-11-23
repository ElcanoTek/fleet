// Package apikeys provides scoped API key management for the fleet orchestrator
// (sched). Ported from moc's internal/apikeys, minus the node-name scope
// patterns: fleet has no node registry (ADR-0011), so a key's authority is its
// permission set plus the typed-key gates (trigger slugs, rate limit, priority
// ceiling) — see ADR-0045.
//
// Spend is deliberately NOT a key attribute. Per-principal dollar/token limits
// live in internal/sched/budget (`scope: key`), which recomputes a window's
// spend from the persisted metering rather than accumulating a counter here —
// one accounting path, so nothing can drift (docs/USAGE-ANALYTICS.md part 2).
package apikeys

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/pgtext"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// APIKey represents a scoped API key.
type APIKey struct {
	KeyID       string              `json:"key_id"`
	Name        string              `json:"name"`
	KeyHash     string              `json:"key_hash"`
	KeyPrefix   string              `json:"key_prefix"`
	Permissions []models.Permission `json:"permissions"`

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

	// UserEmail is the person a user-type key acts as (KeyTypeUser); empty
	// for every other type.
	UserEmail string `json:"user_email,omitempty"`
	// AccountID is the id (core.users.id) of the account a user-type key acts
	// as. CreateBoundUserKey records it when the key is minted; a key minted
	// before keys named their account gets it once, from the account its email
	// had then (BindUserKeys at boot, or the key's first use). The chat edge
	// admits the key only for that account, so a later account created under
	// the same email is never it.
	AccountID string `json:"account_id,omitempty"`
	// AccountMissing records that a user key minted before keys named their
	// account had no account under its email when it was bound. It acts as no
	// one — in particular not as an account created under that email later.
	AccountMissing bool `json:"account_missing,omitempty"`

	// MaxPriority caps the task urgency this key may submit (#230): a task at a
	// priority MORE urgent (lower integer) than this value is rejected. nil =
	// uncapped. Range [models.PriorityMin, models.PriorityMax].
	MaxPriority *int `json:"max_priority,omitempty"`
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
		Permissions:         perms,
		RateLimit:           k.RateLimit,
		CreatedAt:           k.CreatedAt,
		RotatedAt:           k.RotatedAt,
		ExpiresAt:           k.ExpiresAt,
		Enabled:             k.Enabled,
		Description:         k.Description,
		UserEmail:           k.UserEmail,
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
	// stamp identifies the key file's content as of the last load, sync or
	// save — the cheap staleness check behind syncLocked.
	stamp fileStamp
	mu    sync.RWMutex
	// lockWait overrides how long a write waits for the key file's lock
	// (tests); 0 is defaultFileLockWait.
	lockWait time.Duration
	// degraded logs, once, that writes go on without the file lock.
	degraded sync.Once
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

// fileStamp identifies one version of the key file: its mtime and size (one
// Stat, the steady-state check) and its raw bytes (the tie-breaker when
// the Stat looks unchanged but cannot be trusted).
type fileStamp struct {
	mod  time.Time
	size int64
	raw  string
	ok   bool
}

// stampSettle is how long after a write a file's mtime is trusted to tell
// versions apart. A filesystem with coarse timestamps can give two writes in
// the same tick the same mtime (and a revoke-vs-rotate pair can keep the
// size); within this window syncLocked compares content hashes instead.
const stampSettle = 2 * time.Second

func (m *Manager) load() error {
	data, err := os.ReadFile(m.storagePath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	keys, err := parseKeyFile(data)
	if err != nil {
		return err
	}
	m.adoptLocked(keys)
	m.stampLocked(data)
	return nil
}

// parseKeyFile decodes api_keys.json; a malformed record is skipped.
func parseKeyFile(data []byte) ([]APIKey, error) {
	var fileData struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(data, &fileData); err != nil {
		return nil, fmt.Errorf("failed to parse API keys file: %w", err)
	}
	keys := make([]APIKey, 0, len(fileData.Keys))
	for _, keyData := range fileData.Keys {
		var key APIKey
		if err := json.Unmarshal(keyData, &key); err != nil {
			continue
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// adoptLocked makes the file's records the whole in-memory state. An
// existing record is updated IN PLACE (a *APIKey a caller already holds sees
// the change); a record gone from the file is dropped with its rate state;
// the hash index is rebuilt from scratch, so a revoked key's state, a
// rotated-out hash whose grace was cut, or a deleted key's hashes take effect
// at once.
func (m *Manager) adoptLocked(records []APIKey) {
	next := make(map[string]*APIKey, len(records))
	for i := range records {
		rec := records[i]
		if cur, ok := m.keys[rec.KeyID]; ok {
			*cur = rec
			next[rec.KeyID] = cur
		} else {
			next[rec.KeyID] = &rec
		}
	}
	for id := range m.keys {
		if _, ok := next[id]; !ok {
			delete(m.rateLimits, id)
		}
	}
	m.keys = next
	m.keyHashIndex = make(map[string]string, 2*len(next))
	for id, k := range next {
		m.keyHashIndex[k.KeyHash] = id
		if k.PreviousKeyHash != nil {
			m.keyHashIndex[*k.PreviousKeyHash] = id
		}
	}
}

// stampLocked records data (just read or just written) as the file's
// current version.
func (m *Manager) stampLocked(data []byte) {
	st, err := os.Stat(m.storagePath)
	if err != nil {
		m.stamp = fileStamp{}
		return
	}
	m.stamp = fileStamp{mod: st.ModTime(), size: st.Size(), raw: string(data), ok: true}
}

// syncLocked makes this Manager observe every change another process (the
// `fleet sched apikey` CLI, a second Manager) made to the key file — new
// keys, and also revocations, rotations, deletions and edits of keys this
// Manager already knows. It runs with m.mu held at the top of every read and
// write, so a revoked or rotated key stops (and a rotated-in key starts)
// authenticating on the very next request, and no write of ours overwrites a
// change we have not seen. The steady state costs one Stat; the file is read
// only when its mtime or size moved, or its mtime is too recent to trust
// (stampSettle), and re-adopted only when its bytes changed.
//
// A file that cannot be read or parsed leaves the in-memory state as it is:
// writes are atomic renames, so that is a transient or operator error, not a
// reason to forget every key.
func (m *Manager) syncLocked() {
	st, err := os.Stat(m.storagePath)
	if err != nil {
		return
	}
	if m.stamp.ok && st.ModTime().Equal(m.stamp.mod) && st.Size() == m.stamp.size &&
		time.Since(st.ModTime()) > stampSettle {
		return
	}
	data, err := os.ReadFile(m.storagePath)
	if err != nil {
		return
	}
	if m.stamp.ok && string(data) == m.stamp.raw {
		m.stamp.mod, m.stamp.size = st.ModTime(), st.Size()
		return
	}
	records, err := parseKeyFile(data)
	if err != nil {
		return
	}
	m.adoptLocked(records)
	m.stampLocked(data)
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
	// startup (NewManager would refuse to load it). The temp name is unique:
	// the server and a CLI writing at once must not share one file.
	f, err := os.CreateTemp(dir, filepath.Base(m.storagePath)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, m.storagePath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	m.stampLocked(data)
	return nil
}

// lockForWrite takes the key file's lock, then the in-process lock, then
// re-reads the file, so a read-modify-write here cannot lose a change another
// process (the `fleet sched apikey` CLI, the server) made meanwhile. The file
// lock is taken first, on a descriptor of its own and without the in-process
// lock, so a write waiting for another process stalls no key check (they
// need only the in-process lock); a second descriptor conflicts with the
// first even in one process, so writers here still go one at a time. The
// returned function releases both. It fails, holding nothing, when another
// process keeps the file locked past the wait (ErrStoreLocked).
func (m *Manager) lockForWrite() (func(), error) {
	release, err := m.fileLock(m.fileLockWait())
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.syncLocked()
	return func() {
		release()
		m.mu.Unlock()
	}, nil
}

// ErrStoreLocked is a write's error when another process holds the key
// file's lock for longer than the wait (a stopped or hung CLI, say).
var ErrStoreLocked = errors.New("the API-key store is locked by another process; try again")

// defaultFileLockWait bounds how long a write waits for the key file's lock.
const defaultFileLockWait = 5 * time.Second

// SetFileLockWait sets how long a write waits for another process holding
// the key file's lock before it fails with ErrStoreLocked (0 restores
// defaultFileLockWait). Tests shorten it.
func (m *Manager) SetFileLockWait(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lockWait = d
}

func (m *Manager) fileLockWait() time.Duration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.lockWait > 0 {
		return m.lockWait
	}
	return defaultFileLockWait
}

// fileLock takes an exclusive advisory lock (flock) on api_keys.json.lock
// beside the key file, held across every read-modify-write by every process
// that writes the store. It tries without blocking until wait passes (0: one
// try) and then fails with ErrStoreLocked, so a process that keeps the lock
// stalls nobody for long.
//
// The lock file is created mode 0644 whatever the umask, so a lock file a
// root-run CLI created can be locked by the service user; it is opened
// read-write, or read-only when that is refused. When it cannot be opened at
// all, or flock is not supported on it (some network filesystems refuse it on
// a read-only descriptor), writes go on with the in-process lock alone, and
// that is logged once.
func (m *Manager) fileLock(wait time.Duration) (release func(), err error) {
	path := m.storagePath + ".lock"
	noLock := func(why error) (func(), error) {
		m.degraded.Do(func() {
			logging.From(context.Background()).Warn("apikeys: writing the key store without its cross-process lock", "lock_file", path, "err", why)
		})
		return func() {}, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return noLock(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644) //nolint:gosec // G302: an empty lock file; it holds nothing
	if err == nil {
		_ = f.Chmod(0o644) // the umask may have narrowed it; another user's file stays as it is
	} else {
		var rerr error
		if f, rerr = os.Open(path); rerr != nil { //nolint:gosec // G304: the store's own lock file
			return noLock(err)
		}
	}
	deadline := time.Now().Add(wait)
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			_ = f.Close()
			return noLock(err)
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("%w (%s)", ErrStoreLocked, path)
		}
		time.Sleep(25 * time.Millisecond)
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}, nil
}

func (m *Manager) logAudit(ctx context.Context, entry AuditLogEntry) {
	dir := filepath.Dir(m.auditLogPath)
	// 0700: the audit log can reveal key IDs and operations; keep it owner-only.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		logging.From(ctx).Error("apikeys: failed to create audit log dir", "dir", dir, "err", err)
		return
	}
	entry.Timestamp = time.Now().UTC()
	data, err := json.Marshal(entry)
	if err != nil {
		logging.From(ctx).Error("apikeys: failed to marshal audit entry", "err", err)
		return
	}
	f, err := os.OpenFile(m.auditLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		logging.From(ctx).Error("apikeys: failed to open audit log", "path", m.auditLogPath, "err", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		logging.From(ctx).Error("apikeys: failed to append audit entry", "err", err)
	}
}

// CreateKey creates a new API key.
func (m *Manager) CreateKey(ctx context.Context, name string, permissions []models.Permission, role *string, rateLimit int, expiresInDays *int, description string) (*APIKey, string, error) {
	unlock, err := m.lockForWrite()
	if err != nil {
		return nil, "", err
	}
	defer unlock()

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
	// An EMPTY explicit list is treated as "unset", not as "a key with no
	// permissions": a caller that sends `"permissions": []` decodes to a non-nil
	// empty slice, and honouring that would quietly mint a key that can do
	// nothing while reporting success.
	if perms == nil && len(permissions) > 0 {
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

	key := &APIKey{
		KeyID:       keyID,
		Name:        name,
		KeyHash:     keyHash,
		KeyPrefix:   keyPrefix,
		Permissions: perms,
		RateLimit:   rateLimit,
		CreatedAt:   time.Now().UTC(),
		ExpiresAt:   expiresAt,
		Enabled:     true,
		Description: description,
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
	m.logAudit(ctx, AuditLogEntry{
		KeyID:        keyID,
		Action:       "create_key",
		ResourceType: "api_key",
		ResourceID:   &keyID,
		Details: map[string]interface{}{
			"name":        name,
			"permissions": permStrings,
			"rate_limit":  rateLimit,
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
func (m *Manager) CreateTypedKey(ctx context.Context, name string, kt KeyType, allowedTriggerSlugs []string, rateLimit int, expiresInDays *int, description string) (*APIKey, string, error) {
	if kt == KeyTypeUser {
		return nil, "", errors.New("a user key names its user: use CreateUserKey")
	}
	return m.createTypedKey(ctx, name, kt, allowedTriggerSlugs, "", "", rateLimit, expiresInDays, description)
}

// CreateUserKey mints a user-type key (KeyTypeUser) that acts as email on the
// chat API. The account is not checked here: it is resolved on every
// request, so a key for an address that has no enabled account is refused
// then.
//
// A user key takes no per-key rate limit (rateLimit must be 0): RateLimit is
// an hourly request budget ValidateKey charges on EVERY request, which on the
// chat API would count the client's identity checks and config reads, not
// turns — a small value would leave `fleet chat` unable to post. A user key's
// turns are limited as its person's are, by the chat's per-user limiter
// (FLEET_CHAT_RATE_LIMIT_ENABLED, CHAT_RATE_PER_MIN, CHAT_RATE_PER_DAY).
func (m *Manager) CreateUserKey(ctx context.Context, name, email string, rateLimit int, expiresInDays *int, description string) (*APIKey, string, error) {
	email, err := checkUserKey(email, rateLimit)
	if err != nil {
		return nil, "", err
	}
	return m.createTypedKey(ctx, name, KeyTypeUser, nil, email, "", rateLimit, expiresInDays, description)
}

// checkUserKey applies a user key's rules and returns its email as keys store
// it.
func checkUserKey(email string, rateLimit int) (string, error) {
	// Checked as given, then lowercased (pgtext.CheckedEmail): the key acts as
	// this account on every request, so a look-alike must not become the
	// ASCII address it lowercases to.
	checked, ok := pgtext.CheckedEmail(email)
	if !ok {
		return "", fmt.Errorf("a user key's email has a character an address may not hold")
	}
	email = checked
	if email == "" || !strings.Contains(email, "@") {
		return "", fmt.Errorf("a user key needs the email of the account it acts as")
	}
	if rateLimit != 0 {
		return "", fmt.Errorf("a user key takes no per-key rate limit: its turns are limited by the chat's per-user limit, like its person's")
	}
	return email, nil
}

func (m *Manager) createTypedKey(ctx context.Context, name string, kt KeyType, allowedTriggerSlugs []string, userEmail, accountID string, rateLimit int, expiresInDays *int, description string) (*APIKey, string, error) {
	if !kt.Valid() {
		return nil, "", fmt.Errorf("invalid key type: %q", kt)
	}
	unlock, err := m.lockForWrite()
	if err != nil {
		return nil, "", err
	}
	defer unlock()

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
		UserEmail:           userEmail,
		AccountID:           accountID,
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
	m.logAudit(ctx, AuditLogEntry{
		KeyID:        keyID,
		Action:       "create_key",
		ResourceType: "api_key",
		ResourceID:   &keyID,
		Details: map[string]interface{}{
			"name":                  name,
			"type":                  string(kt),
			"allowed_trigger_slugs": allowedTriggerSlugs,
			"user_email":            userEmail,
			"permissions":           permStrings,
			"rate_limit":            rateLimit,
		},
		Success: true,
	})
	return key, rawKey, nil
}

// RotateKey rotates an API key, keeping the old key valid for a grace period.
func (m *Manager) RotateKey(ctx context.Context, keyID string, gracePeriodHours int) (*APIKey, string, error) {
	unlock, err := m.lockForWrite()
	if err != nil {
		return nil, "", err
	}
	defer unlock()

	key, ok := m.keys[keyID]
	if !ok {
		return nil, "", fmt.Errorf("key %s not found", keyID)
	}
	// Preserve the key's type across rotation (#190): a typed key rotates to a
	// fresh token of the SAME type, so its scope is unchanged; a legacy (sk-) key
	// stays legacy.
	var rawKey, keyHash, keyPrefix string
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
	// Retire the outgoing previous hash from the lookup index BEFORE it is
	// overwritten below (#567). Only the most recent predecessor may keep
	// authenticating within its grace window: without this delete, a second
	// rotation forgot the first rotated-out hash was still indexed, and
	// ValidateKey's grace-expiry cleanup (which only checks the CURRENT
	// PreviousKeyHash) could never evict it — so the original, possibly leaked,
	// key kept authenticating until a process restart. Rotation must actually
	// revoke prior key material.
	if key.PreviousKeyHash != nil {
		delete(m.keyHashIndex, *key.PreviousKeyHash)
	}
	key.PreviousKeyHash = &oldHash
	key.PreviousKeyExpires = &graceExpires
	key.KeyHash = keyHash
	key.KeyPrefix = keyPrefix
	key.RotatedAt = &now
	m.keyHashIndex[keyHash] = keyID
	if err := m.save(); err != nil {
		return nil, "", err
	}
	m.logAudit(ctx, AuditLogEntry{
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
func (m *Manager) RevokeKey(ctx context.Context, keyID string) error {
	unlock, err := m.lockForWrite()
	if err != nil {
		return err
	}
	defer unlock()
	key, ok := m.keys[keyID]
	if !ok {
		return fmt.Errorf("key %s not found", keyID)
	}
	key.Enabled = false
	if err := m.save(); err != nil {
		return err
	}
	m.logAudit(ctx, AuditLogEntry{KeyID: keyID, Action: "revoke_key", ResourceType: "api_key", ResourceID: &keyID, Success: true})
	return nil
}

// DeleteKey permanently deletes an API key.
func (m *Manager) DeleteKey(ctx context.Context, keyID string) error {
	unlock, err := m.lockForWrite()
	if err != nil {
		return err
	}
	defer unlock()
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
	m.logAudit(ctx, AuditLogEntry{KeyID: keyID, Action: "delete_key", ResourceType: "api_key", ResourceID: &keyID, Success: true})
	return nil
}

// ErrNoAccount refuses a user key for an address that has no account: a user
// key is bound to the account it acts as, so the account comes first.
var ErrNoAccount = errors.New("a user key acts as an existing account: create the account first")

// CreateBoundUserKey mints a user key (CreateUserKey's rules) bound to the
// account it acts as: email and that account's id, which the caller looked
// up. Every fleet command that mints a user key uses it. The key is written
// once, with its account.
func (m *Manager) CreateBoundUserKey(ctx context.Context, name, email, accountID string, rateLimit int, expiresInDays *int, description string) (*APIKey, string, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil, "", ErrNoAccount
	}
	email, err := checkUserKey(email, rateLimit)
	if err != nil {
		return nil, "", err
	}
	return m.createTypedKey(ctx, name, KeyTypeUser, nil, email, accountID, rateLimit, expiresInDays, description)
}

// RecordUserKeyAccount binds a user key that names no account yet to
// accountID, or, with accountID "", records that its email had no account
// (AccountMissing): from then on the key acts as no one. A key is bound once;
// recording the same account again is a no-op, any other change an error.
func (m *Manager) RecordUserKeyAccount(ctx context.Context, keyID, accountID string) error {
	accountID = strings.TrimSpace(accountID)
	unlock, err := m.lockForWrite()
	if err != nil {
		return err
	}
	defer unlock()
	key, ok := m.keys[keyID]
	if !ok {
		return fmt.Errorf("key %s not found", keyID)
	}
	if key.Type != KeyTypeUser {
		return fmt.Errorf("key %s is not a user key", keyID)
	}
	switch {
	case key.AccountID == accountID && !key.AccountMissing && accountID != "":
		return nil
	case accountID == "" && key.AccountMissing && key.AccountID == "":
		// Already recorded as having no account (another first use got there).
		return nil
	case key.AccountID != "" || key.AccountMissing:
		return fmt.Errorf("key %s is already bound", keyID)
	}
	key.AccountID, key.AccountMissing = accountID, accountID == ""
	if err := m.save(); err != nil {
		key.AccountID, key.AccountMissing = "", false
		return err
	}
	m.logAudit(ctx, AuditLogEntry{KeyID: keyID, Action: "bind_key", ResourceType: "api_key", ResourceID: &keyID,
		Details: map[string]interface{}{"account_id": accountID, "account_missing": accountID == ""}, Success: true})
	return nil
}

// AccountLookup finds the account under email: its id, or found=false when
// there is none. An error means the answer is not known.
type AccountLookup func(ctx context.Context, email string) (id string, found bool, err error)

// BindUserKeys binds every user key minted before keys named their account
// (no AccountID, not AccountMissing) to the account its email has now, or
// marks it AccountMissing when there is none. fleet serve runs it at boot, so
// the binding reflects the accounts of the moment keys started naming theirs.
// A key whose lookup fails is left for the next boot or its first use. The
// lookups run without the key store's lock; the results are written in one
// save.
func (m *Manager) BindUserKeys(ctx context.Context, lookup AccountLookup) (bound, missing int, err error) {
	type pending struct{ keyID, email string }
	m.mu.Lock()
	m.syncLocked()
	var todo []pending
	for _, key := range m.keys {
		if key.Type == KeyTypeUser && key.AccountID == "" && !key.AccountMissing {
			todo = append(todo, pending{key.KeyID, key.UserEmail})
		}
	}
	m.mu.Unlock()
	if len(todo) == 0 {
		return 0, 0, nil
	}
	found := map[string]string{} // keyID → account id ("" = no account)
	for _, p := range todo {
		id, ok, lerr := lookup(ctx, p.email)
		if lerr != nil {
			continue
		}
		if !ok {
			id = ""
		}
		found[p.keyID] = id
	}
	unlock, err := m.lockForWrite()
	if err != nil {
		return 0, 0, err
	}
	defer unlock()
	var changed []*APIKey
	for _, p := range todo {
		id, decided := found[p.keyID]
		key, ok := m.keys[p.keyID]
		if !decided || !ok || key.AccountID != "" || key.AccountMissing || key.UserEmail != p.email {
			continue
		}
		key.AccountID, key.AccountMissing = id, id == ""
		changed = append(changed, key)
		if id == "" {
			missing++
		} else {
			bound++
		}
	}
	if len(changed) == 0 {
		return 0, 0, nil
	}
	if err := m.save(); err != nil {
		for _, key := range changed {
			key.AccountID, key.AccountMissing = "", false
		}
		return 0, 0, err
	}
	for _, key := range changed {
		keyID := key.KeyID
		m.logAudit(ctx, AuditLogEntry{KeyID: keyID, Action: "bind_key", ResourceType: "api_key", ResourceID: &keyID,
			Details: map[string]interface{}{"account_id": key.AccountID, "account_missing": key.AccountMissing}, Success: true})
	}
	return bound, missing, nil
}

// DeleteUserKeys permanently deletes the user-type keys (KeyTypeUser) of the
// account accountID, which was email's, and reports how many it deleted.
// Deleting an account calls it once the account's rows are gone
// (internal/accountdelete): a user key acts only as its account, so it has
// nothing left to do. It deletes
//
//   - every user key bound to accountID (APIKey.AccountID), and
//   - the user keys for email that name no account (minted before keys named
//     their account, or marked as having none) — but only while no account
//     holds email: emailLive is asked under the key store's lock, which a
//     key's first-use binding (RecordUserKeyAccount) also takes.
//
// A key bound to any other account — one created under email after this
// one was deleted — is never touched; nor are keys of every other type,
// which name no account. The audit log keeps a delete_key entry per key.
func (m *Manager) DeleteUserKeys(ctx context.Context, email, accountID string, emailLive func(context.Context) (bool, error)) (int, error) {
	// Both sides through the one email key (pgtext.EmailKey).
	email = pgtext.EmailKey(email)
	if email == "" {
		return 0, nil
	}
	unlock, err := m.lockForWrite()
	if err != nil {
		return 0, err
	}
	defer unlock()
	ofEmail := func(key *APIKey) bool {
		return key.Type == KeyTypeUser && pgtext.EmailKey(key.UserEmail) == email
	}
	unboundLeft := false
	for _, key := range m.keys {
		if ofEmail(key) && key.AccountID == "" {
			unboundLeft = true
			break
		}
	}
	takeUnbound := false
	if unboundLeft {
		live := true // with no way to ask, an unbound key is left alone
		if emailLive != nil {
			if live, err = emailLive(ctx); err != nil {
				return 0, err
			}
		}
		takeUnbound = !live
	}
	var gone []*APIKey
	for _, key := range m.keys {
		if !ofEmail(key) {
			continue
		}
		if (accountID != "" && key.AccountID == accountID) || (key.AccountID == "" && takeUnbound) {
			gone = append(gone, key)
		}
	}
	if len(gone) == 0 {
		return 0, nil
	}
	for _, key := range gone {
		delete(m.keys, key.KeyID)
		delete(m.keyHashIndex, key.KeyHash)
		if key.PreviousKeyHash != nil {
			delete(m.keyHashIndex, *key.PreviousKeyHash)
		}
	}
	if err := m.save(); err != nil {
		// The file still holds the keys: keep memory in step with it.
		for _, key := range gone {
			m.keys[key.KeyID] = key
			m.keyHashIndex[key.KeyHash] = key.KeyID
			if key.PreviousKeyHash != nil {
				m.keyHashIndex[*key.PreviousKeyHash] = key.KeyID
			}
		}
		return 0, err
	}
	for _, key := range gone {
		delete(m.rateLimits, key.KeyID)
		keyID := key.KeyID
		m.logAudit(ctx, AuditLogEntry{KeyID: keyID, Action: "delete_key", ResourceType: "api_key", ResourceID: &keyID,
			Details: map[string]interface{}{"reason": "account deleted"}, Success: true})
	}
	return len(gone), nil
}

// ValidateKey validates an API key and checks permissions, on no particular
// surface. HTTP edges must not call it: they call ValidateKeyOn, which also
// refuses a key type that does not authenticate on that surface
// (KeyType.AllowsSurface; TestKeyEdgesNameTheirSurface pins this).
func (m *Manager) ValidateKey(ctx context.Context, rawKey string, requiredPermission *models.Permission, ipAddress, userAgent *string) (bool, *APIKey, string) {
	return m.validate(ctx, rawKey, "", requiredPermission, ipAddress, userAgent)
}

// ReasonWrongSurface is ValidateKeyOn's reason for a key whose type does not
// authenticate on the surface. An edge may answer it like an unknown key, or
// name the key type the surface wants.
const ReasonWrongSurface = "this API key type does not authenticate here"

// ValidateKeyOn is ValidateKey for a request on surface s: a key whose type
// does not authenticate there is refused (ReasonWrongSurface) before any
// rate-limit token is charged or a permission checked, so no edge has to
// remember which key types to turn away.
func (m *Manager) ValidateKeyOn(ctx context.Context, s Surface, rawKey string, requiredPermission *models.Permission, ipAddress, userAgent *string) (bool, *APIKey, string) {
	return m.validate(ctx, rawKey, s, requiredPermission, ipAddress, userAgent)
}

func (m *Manager) validate(ctx context.Context, rawKey string, surface Surface, requiredPermission *models.Permission, ipAddress, userAgent *string) (bool, *APIKey, string) {
	keyHash := m.hashKey(rawKey)

	m.mu.Lock()
	defer m.mu.Unlock()
	m.syncLocked()

	keyID, ok := m.keyHashIndex[keyHash]
	if !ok {
		return false, nil, "Invalid API key"
	}
	key, ok := m.keys[keyID]
	if !ok {
		return false, nil, "Invalid API key"
	}
	if surface != "" && !key.Type.AllowsSurface(surface) {
		return false, nil, ReasonWrongSurface
	}
	if key.PreviousKeyHash != nil && keyHash == *key.PreviousKeyHash {
		if key.PreviousKeyExpires != nil && time.Now().UTC().After(*key.PreviousKeyExpires) {
			// Retire it in memory at once: the rotated-out token is refused
			// here whatever happens next.
			delete(m.keyHashIndex, keyHash)
			key.PreviousKeyHash = nil
			key.PreviousKeyExpires = nil
			// Persist it only if the file's lock is free right now:
			// authentication never waits on another process. Under the lock
			// the file is re-read first; another process's change to the key
			// is adopted (then retired again here if it still holds this
			// token). The key is saved when it no longer has a rotated-out
			// token — which an unchanged file still records. When skipped,
			// the next write of the store saves it, and the expiry is checked
			// on every use meanwhile.
			if release, err := m.fileLock(0); err == nil {
				m.syncLocked()
				if cur, ok := m.keys[keyID]; ok {
					if cur.PreviousKeyHash != nil && *cur.PreviousKeyHash == keyHash {
						delete(m.keyHashIndex, keyHash)
						cur.PreviousKeyHash = nil
						cur.PreviousKeyExpires = nil
					}
					if cur.PreviousKeyHash == nil {
						if err := m.save(); err != nil {
							logging.From(ctx).Error("apikeys: failed to persist expiry of rotated key", "key_id", cur.KeyID, "err", err)
						}
					}
				}
				release()
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
		m.logAudit(ctx, AuditLogEntry{KeyID: keyID, Action: "validate_key", ResourceType: "api_key", ResourceID: &keyID, IPAddress: ipAddress, UserAgent: userAgent, Success: false, ErrorMessage: &errMsg})
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
			m.logAudit(ctx, AuditLogEntry{KeyID: keyID, Action: "rate_limit_exceeded", ResourceType: "api_key", ResourceID: &keyID, IPAddress: ipAddress, UserAgent: userAgent, Success: false, ErrorMessage: &errMsg})
			return false, nil, fmt.Sprintf("Rate limit exceeded (%d requests/hour)", key.RateLimit)
		}
		state.Increment()
	}

	if requiredPermission != nil && !key.HasPermission(*requiredPermission) {
		errMsg := fmt.Sprintf("Missing permission: %s", *requiredPermission)
		m.logAudit(ctx, AuditLogEntry{KeyID: keyID, Action: "permission_denied", ResourceType: "api_key", ResourceID: &keyID, Details: map[string]interface{}{"required_permission": string(*requiredPermission)}, IPAddress: ipAddress, UserAgent: userAgent, Success: false, ErrorMessage: &errMsg})
		return false, nil, errMsg
	}

	// A copy: the caller reads it after the lock is released, while a
	// concurrent request may bind the record (RecordUserKeyAccount).
	k := *key
	return true, &k, ""
}

// LookupKeyMeta resolves a raw key to its stable key ID and per-key rate-limit
// override WITHOUT enforcing or mutating any rate-limit state — a read-only
// helper for the orchestrator's sliding-window task limiter (#247), which keys
// its window by the stable key ID and treats RateLimit (when > 0) as that key's
// per-minute cap. Returns ok=false for unknown, disabled, or expired keys, and
// for a key whose type does not authenticate on surface s.
func (m *Manager) LookupKeyMetaOn(s Surface, rawKey string) (keyID string, rateLimit int, ok bool) {
	key := m.lookup(s, rawKey)
	if key == nil {
		return "", 0, false
	}
	return key.KeyID, key.RateLimit, true
}

// LookupKeyTypeOn resolves a raw key to its access class and whether it
// carries task-create permission, WITHOUT enforcing or mutating any
// rate-limit state (#190) — a read-only helper for the task-create paths'
// under-scope gate. Returns ok=false for unknown, disabled, or expired keys,
// and for a key whose type does not authenticate on surface s. A legacy
// (untyped) key returns KeyTypeLegacy, so callers can preserve historical
// behavior for it.
func (m *Manager) LookupKeyTypeOn(s Surface, rawKey string) (kt KeyType, hasCreate, ok bool) {
	key := m.lookup(s, rawKey)
	if key == nil {
		return "", false, false
	}
	return key.Type, key.HasPermission(models.PermissionCreateTask), true
}

// LookupKeyMeta is LookupKeyMetaOn on no particular surface (tests, tools).
func (m *Manager) LookupKeyMeta(rawKey string) (keyID string, rateLimit int, ok bool) {
	return m.LookupKeyMetaOn("", rawKey)
}

// LookupKeyType is LookupKeyTypeOn on no particular surface (tests, tools).
func (m *Manager) LookupKeyType(rawKey string) (kt KeyType, hasCreate, ok bool) {
	return m.LookupKeyTypeOn("", rawKey)
}

// lookup is the read-only key resolution behind the Lookup helpers: a copy
// of the valid key rawKey names, or nil.
func (m *Manager) lookup(s Surface, rawKey string) *APIKey {
	keyHash := m.hashKey(rawKey)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.syncLocked()
	id, found := m.keyHashIndex[keyHash]
	if !found {
		return nil
	}
	key, found := m.keys[id]
	if !found || !key.IsValid() || (s != "" && !key.Type.AllowsSurface(s)) {
		return nil
	}
	if key.PreviousKeyHash != nil && keyHash == *key.PreviousKeyHash &&
		key.PreviousKeyExpires != nil && time.Now().UTC().After(*key.PreviousKeyExpires) {
		return nil // a rotated-out key past its grace
	}
	k := *key
	return &k
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
	m.syncLocked()
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

// SetMaxPriority sets (or clears, when max is nil) a key's task-urgency ceiling
// (#230) and persists it. Used by the create/update handlers. Returns an error
// for an unknown key.
func (m *Manager) SetMaxPriority(keyID string, ceiling *int) error {
	unlock, err := m.lockForWrite()
	if err != nil {
		return err
	}
	defer unlock()
	key, ok := m.keys[keyID]
	if !ok {
		return fmt.Errorf("api key not found: %s", keyID)
	}
	key.MaxPriority = ceiling
	return m.save()
}

// GetKey gets a key by ID.
func (m *Manager) GetKey(keyID string) *APIKey {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.syncLocked()
	return m.keys[keyID]
}

// ListKeys lists all API keys.
func (m *Manager) ListKeys() []*APIKey {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.syncLocked()
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
