package store

import (
	"context"
	"time"
)

// Workspace feature settings (migration 035): admin overrides for the curated
// runtime settings registry (internal/settings). The table stores OVERRIDES
// only — an absent key means "use the env-derived default" — so the store layer
// is a plain string k/v; all typing, validation, and the registry of legal keys
// live in internal/settings, and the values carry no secret material.

// WorkspaceSetting is one admin override row.
type WorkspaceSetting struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	UpdatedAt int64  `json:"updated_at"`
	UpdatedBy string `json:"updated_by"`
}

// WorkspaceSettings returns every override row, keyed by setting key.
func (s *Store) WorkspaceSettings(ctx context.Context) (map[string]WorkspaceSetting, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, value, updated_at, updated_by FROM workspace_settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]WorkspaceSetting{}
	for rows.Next() {
		var ws WorkspaceSetting
		if err := rows.Scan(&ws.Key, &ws.Value, &ws.UpdatedAt, &ws.UpdatedBy); err != nil {
			return nil, err
		}
		out[ws.Key] = ws
	}
	return out, rows.Err()
}

// SetWorkspaceSetting upserts one override and returns the persisted row, so
// the caller can report exactly what was written without a second read. The
// caller (internal/settings) has already validated key and value against the
// registry.
func (s *Store) SetWorkspaceSetting(ctx context.Context, key, value, updatedBy string) (WorkspaceSetting, error) {
	var ws WorkspaceSetting
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO workspace_settings (key, value, updated_at, updated_by)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (key) DO UPDATE
		SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at, updated_by = EXCLUDED.updated_by
		RETURNING key, value, updated_at, updated_by`,
		key, value, time.Now().Unix(), updatedBy).
		Scan(&ws.Key, &ws.Value, &ws.UpdatedAt, &ws.UpdatedBy)
	return ws, err
}

// DeleteWorkspaceSetting removes one override, reverting the setting to its
// env-derived default. Deleting an absent key is a no-op, not an error — the
// end state ("no override") is what the caller asked for.
func (s *Store) DeleteWorkspaceSetting(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM workspace_settings WHERE key = $1`, key)
	return err
}
