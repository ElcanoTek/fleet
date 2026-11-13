package models

import (
	"strings"
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

// TaskModelRollout describes an operator rollout of primary and/or fallback
// models across upcoming scheduled and pending tasks (and in-flight recurring
// chains when they spawn their next occurrence).
type TaskModelRollout struct {
	// Model, when non-nil and non-empty, updates the primary model on
	// matching tasks.
	Model *string `json:"model,omitempty"`
	// FromModel restricts primary-model updates to tasks currently pinned
	// to FromModel (empty updates all matching tasks whose model differs).
	FromModel string `json:"from_model,omitempty"`
	// FallbackModel, when non-nil, updates fallback_model on matching
	// tasks ("" clears fallback_model to NULL).
	FallbackModel *string `json:"fallback_model,omitempty"`
	// FromFallbackModel restricts fallback_model updates to tasks currently
	// using FromFallbackModel (empty updates all matching tasks whose
	// fallback_model differs).
	FromFallbackModel string `json:"from_fallback_model,omitempty"`
	// OnlySetFallbacks, when true alongside FallbackModel, updates only tasks
	// that already have a non-empty fallback_model, preserving NULL on bare
	// API/CLI tasks created without a task fallback.
	OnlySetFallbacks bool `json:"only_set_fallbacks,omitempty"`
	// RecurringOnly, when true, restricts updates to tasks with a non-empty
	// cron recurrence.
	RecurringOnly bool `json:"recurring_only,omitempty"`
}

// ApplyToPointers computes the post-rollout (model, fallbackModel) pointers for
// a single task and reports whether primary and/or fallback changed.
func (r TaskModelRollout) ApplyToPointers(curModel, curFallback *string) (nextModel, nextFallback *string, primaryChanged, fallbackChanged bool) {
	nextModel = cloneOptionalTrimmed(curModel)
	nextFallback = cloneOptionalTrimmed(curFallback)

	curM := derefTrimmed(curModel)
	if r.Model != nil {
		targetM := strings.TrimSpace(*r.Model)
		fromM := strings.TrimSpace(r.FromModel)
		if targetM != "" && (fromM == "" || curM == fromM) && curM != targetM {
			v := targetM
			nextModel = &v
			primaryChanged = true
		}
	}

	curFB := derefTrimmed(curFallback)
	if r.FallbackModel != nil {
		targetFB := strings.TrimSpace(*r.FallbackModel)
		fromFB := strings.TrimSpace(r.FromFallbackModel)
		if (!r.OnlySetFallbacks || curFB != "") && (fromFB == "" || curFB == fromFB) {
			if targetFB == "" {
				if curFB != "" {
					nextFallback = nil
					fallbackChanged = true
				}
			} else if curFB != targetFB {
				v := targetFB
				nextFallback = &v
				fallbackChanged = true
			}
		}
	}
	return nextModel, nextFallback, primaryChanged, fallbackChanged
}

func derefTrimmed(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

func cloneOptionalTrimmed(p *string) *string {
	v := derefTrimmed(p)
	if v == "" {
		return nil
	}
	return &v
}

// TaskModelRolloutResult summarizes a TaskModelRollout dry-run or execution.
type TaskModelRolloutResult struct {
	DryRun                bool    `json:"dry_run"`
	ScannedCount          int     `json:"scanned_count"`
	UpdatedCount          int     `json:"updated_count"`
	PrimaryUpdatedCount   int     `json:"primary_updated_count"`
	FallbackUpdatedCount  int     `json:"fallback_updated_count"`
	RecurringUpdatedCount int     `json:"recurring_updated_count"`
	Tasks                 []*Task `json:"tasks,omitempty"`
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
