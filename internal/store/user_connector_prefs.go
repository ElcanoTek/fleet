package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Per-user connector availability preferences (unified connector UX). A row is
// an EXPLICIT user choice; absence means "operator default" so the feature is
// a no-op until a user touches a toggle. Prefs shape what the chat pickers
// offer and which credential-account seat a chat turn uses by default — they
// are NOT an authority boundary (the credential allowlist and sharing grants
// stay the security gates, and a task's pinned mcp_selection is never
// rewritten by a later pref change).

// Connector kinds a preference row may reference.
const (
	ConnectorKindBundled = "bundled" // sandboxed manifest connector, keyed by server name
	ConnectorKindRemote  = "remote"  // per-user hosted connection (own or shared), keyed by server id
)

// ErrConnectorPrefInvalid is returned for a malformed preference write.
var ErrConnectorPrefInvalid = errors.New("invalid connector preference")

// ConnectorPref is one explicit per-user availability choice.
type ConnectorPref struct {
	Kind           string `json:"kind"`
	ConnectorID    string `json:"connector_id"`
	Enabled        bool   `json:"enabled"`
	DefaultAccount string `json:"default_account,omitempty"`
	UpdatedAt      int64  `json:"updated_at"`
}

// SetConnectorPref upserts one preference row.
func (s *Store) SetConnectorPref(ctx context.Context, userEmail string, p ConnectorPref) error {
	email := normalizeEmail(userEmail)
	kind := strings.TrimSpace(p.Kind)
	id := strings.TrimSpace(p.ConnectorID)
	if email == "" || id == "" || (kind != ConnectorKindBundled && kind != ConnectorKindRemote) {
		return fmt.Errorf("%w: kind=%q connector_id=%q", ErrConnectorPrefInvalid, p.Kind, p.ConnectorID)
	}
	if kind == ConnectorKindRemote && p.DefaultAccount != "" {
		// Seats are a bundled-connector concept (host-brokered credential
		// accounts); a remote connection is one account by construction.
		return fmt.Errorf("%w: default_account applies only to bundled connectors", ErrConnectorPrefInvalid)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO user_connector_prefs (user_email, connector_kind, connector_id, enabled, default_account, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (user_email, connector_kind, connector_id)
		DO UPDATE SET enabled = EXCLUDED.enabled, default_account = EXCLUDED.default_account, updated_at = EXCLUDED.updated_at`,
		email, kind, id, p.Enabled, strings.TrimSpace(p.DefaultAccount), time.Now().Unix())
	return err
}

// DeleteConnectorPref removes an explicit choice, reverting the connector to
// its operator default. Deleting an absent row is a no-op (idempotent revert).
func (s *Store) DeleteConnectorPref(ctx context.Context, userEmail, kind, connectorID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM user_connector_prefs WHERE user_email = $1 AND connector_kind = $2 AND connector_id = $3`,
		normalizeEmail(userEmail), strings.TrimSpace(kind), strings.TrimSpace(connectorID))
	return err
}

// ListConnectorPrefs returns all of a user's explicit choices, keyed by
// kind+"\x00"+connector_id for O(1) lookup by callers.
func (s *Store) ListConnectorPrefs(ctx context.Context, userEmail string) (map[string]ConnectorPref, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT connector_kind, connector_id, enabled, default_account, updated_at
		FROM user_connector_prefs WHERE user_email = $1`,
		normalizeEmail(userEmail))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ConnectorPref{}
	for rows.Next() {
		var p ConnectorPref
		if err := rows.Scan(&p.Kind, &p.ConnectorID, &p.Enabled, &p.DefaultAccount, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out[ConnectorPrefKey(p.Kind, p.ConnectorID)] = p
	}
	return out, rows.Err()
}

// ConnectorPrefKey is the lookup key ListConnectorPrefs maps by.
func ConnectorPrefKey(kind, connectorID string) string {
	return kind + "\x00" + connectorID
}
