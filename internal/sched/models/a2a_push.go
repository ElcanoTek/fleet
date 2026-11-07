package models

import (
	"time"

	"github.com/google/uuid"
)

// ── A2A push-notification configs (#1279 Phase 2) ──

// A2APushConfig is one per-task webhook registration made by an external A2A
// caller: when the task's caller-visible state changes, the push dispatcher
// POSTs an A2A StreamResponse to URL. Token and AuthCredentials are the
// CALLER's secrets for authenticating fleet's webhook calls to the caller's
// receiver — they are sealed at rest (internal/secretbox) and carried in
// plaintext only in memory, between unsealing and the HTTP request.
type A2APushConfig struct {
	// TaskID + ID form the identity. ID is client-supplied when the caller
	// gave one (the A2A spec lets a caller manage multiple configs per task
	// under its own ids), server-minted otherwise.
	TaskID uuid.UUID `json:"task_id"`
	ID     string    `json:"id"`
	URL    string    `json:"url"`
	// Token is echoed to the receiver in the notification-token headers.
	Token string `json:"-"`
	// AuthScheme/AuthCredentials render the Authorization header VERBATIM as
	// "{scheme} {credentials}" — the scheme's casing is preserved exactly as
	// registered, because receivers (the A2A TCK included) compare strings.
	AuthScheme      string `json:"auth_scheme,omitempty"`
	AuthCredentials string `json:"-"`
	// LastPushedStatus is the delivery marker: the fleet task status this
	// config last had a delivery ATTEMPT for. nil = never pushed.
	LastPushedStatus *string   `json:"last_pushed_status,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// A2APushWork is one due delivery: a config whose task has moved to a status
// the config has not been pushed yet. Secrets arrive unsealed (memory-only).
type A2APushWork struct {
	Config A2APushConfig
	// Status is the task's CURRENT status — what the delivery reports and
	// what MarkA2APushAttempted records afterwards.
	Status TaskStatus
}
