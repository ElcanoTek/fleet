package models

import (
	"time"
)

// APIKeyCreate is the request model for creating an API key.
type APIKeyCreate struct {
	Name string `json:"name"`
	// Type, when set, mints a typed key (#190): one of admin|task|webhook|readonly.
	// When empty, the legacy role-based path is used. A webhook key requires
	// AllowedTriggerSlugs.
	Type                string   `json:"type,omitempty"`
	AllowedTriggerSlugs []string `json:"allowed_trigger_slugs,omitempty"`
	Role                *string  `json:"role,omitempty"`
	// Permissions is an explicit permission set for the legacy (role-based)
	// path, for the grants no role expresses — chiefly view_all_logs, the
	// fleet-wide transcript read (#980). It is mutually exclusive with both Role
	// and Type: a typed key derives its permissions from its type, and silently
	// ignoring the field on either path is how an operator ends up believing
	// they minted an auditor key that in fact reads nothing.
	Permissions   []Permission `json:"permissions,omitempty"`
	RateLimit     int          `json:"rate_limit"`
	ExpiresInDays *int         `json:"expires_in_days,omitempty"`
	Description   string       `json:"description"`
	// MaxCostPerDayUSD / MaxCostPerMonthUSD are RETIRED per-key spending caps,
	// kept on the request model for one purpose: so CreateAPIKey can answer a
	// caller that still sends them with a 400 naming the replacement
	// (POST /admin/budgets, scope=key) instead of accepting a cap it would
	// never enforce. They are never persisted.
	MaxCostPerDayUSD   *float64 `json:"max_cost_per_day_usd,omitempty"`
	MaxCostPerMonthUSD *float64 `json:"max_cost_per_month_usd,omitempty"`
	// MaxPriority caps how urgent a task this key may submit (#230): the key
	// cannot create a task at a priority MORE urgent (lower integer) than this.
	// nil = uncapped. Range [PriorityMin, PriorityMax].
	MaxPriority *int `json:"max_priority,omitempty"`
}

// APIKeyResponse is the response model for API key operations.
type APIKeyResponse struct {
	KeyID               string     `json:"key_id"`
	Name                string     `json:"name"`
	KeyPrefix           string     `json:"key_prefix"`
	Type                string     `json:"type,omitempty"`
	AllowedTriggerSlugs []string   `json:"allowed_trigger_slugs,omitempty"`
	Permissions         []string   `json:"permissions"`
	RateLimit           int        `json:"rate_limit"`
	CreatedAt           time.Time  `json:"created_at"`
	RotatedAt           *time.Time `json:"rotated_at,omitempty"`
	ExpiresAt           *time.Time `json:"expires_at,omitempty"`
	Enabled             bool       `json:"enabled"`
	Description         string     `json:"description"`
	// UserEmail is the account a user-type key acts as (ADR-0108).
	UserEmail string `json:"user_email,omitempty"`
	// MaxPriority is the key's task-urgency ceiling (#230); nil = uncapped.
	MaxPriority *int `json:"max_priority,omitempty"`
}

// APIKeyCreated is the response model when a new key is created.
type APIKeyCreated struct {
	APIKeyResponse
	APIKey string `json:"api_key"`
}

// APIKeyRotated is the response model when a key is rotated.
type APIKeyRotated struct {
	APIKeyResponse
	APIKey           string `json:"api_key"`
	GracePeriodHours int    `json:"grace_period_hours"`
}

// AuditLogResponse is the response model for audit log entries.
type AuditLogResponse struct {
	Entries []map[string]interface{} `json:"entries"`
	Total   int                      `json:"total"`
}

// HealthResponse is the health check response.
type HealthResponse struct {
	Status    string `json:"status"`
	Version   string `json:"version"`
	Timestamp string `json:"timestamp"`
}

// CleanupResponse is the response for task cleanup.
type CleanupResponse struct {
	DeletedCount int `json:"deleted_count"`
}

// BulkModelUpdate is the POST /tasks/model request: re-assign the pinned model
// (and optional fallback) across scheduled tasks. FromModel, when set, limits the
// change to tasks currently pinned to that slug (e.g. a deprecated model). DryRun
// returns the tasks that WOULD change without writing.
type BulkModelUpdate struct {
	Model string `json:"model"`
	// FallbackModel is optional: omit to keep each task's existing
	// fallback; send "" to clear it to NULL; send a slug to set it (#1120).
	FallbackModel *string `json:"fallback_model"`
	FromModel     string  `json:"from_model"`
	DryRun        bool    `json:"dry_run"`
}

// BulkModelUpdateResult is the POST /tasks/model response. On a dry run it lists
// the matched tasks + count without writing; on a real run it reports how many
// were updated.
type BulkModelUpdateResult struct {
	DryRun       bool    `json:"dry_run"`
	UpdatedCount int     `json:"updated_count,omitempty"`
	MatchedCount int     `json:"matched_count,omitempty"`
	Tasks        []*Task `json:"tasks,omitempty"`
}

// DeleteKeyResponse is the response for API key deletion.
type DeleteKeyResponse struct {
	Deleted bool   `json:"deleted"`
	KeyID   string `json:"key_id"`
}

// ErrorResponse is the standard error response.
type ErrorResponse struct {
	Detail string `json:"detail"`
}

// FileInfo contains metadata about an uploaded file including its checksum.
type FileInfo struct {
	Filename string `json:"filename"`
	Checksum string `json:"checksum"`
	Size     int64  `json:"size"`
}

// PaginatedResponse wraps paginated results with metadata.
type PaginatedResponse struct {
	Data   interface{} `json:"data"`
	Total  int         `json:"total"`
	Limit  int         `json:"limit"`
	Offset int         `json:"offset"`
}
