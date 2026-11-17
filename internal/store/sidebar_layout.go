package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Saved left-rail ("sidebar") layouts (migration 0202): one row per user in
// chat.sidebar_layouts and a singleton workspace default in
// chat.workspace_sidebar_layout. The store keeps the layout as opaque JSON;
// internal/sidebar validates it before a write and again after a read (a row
// saved by an older build may no longer pass), so nothing here parses it.

// ErrSidebarLayoutNotFound reports that no layout is saved (for the user, or
// for the workspace).
var ErrSidebarLayoutNotFound = errors.New("sidebar layout not found")

// WorkspaceSidebarLayout is the admin-saved workspace default layout.
type WorkspaceSidebarLayout struct {
	Layout    []byte // JSON
	UpdatedBy string
	UpdatedAt int64 // unix seconds
}

// SidebarLayout returns the user's saved layout JSON, or
// ErrSidebarLayoutNotFound.
func (s *Store) SidebarLayout(ctx context.Context, userEmail string) ([]byte, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT layout FROM chat.sidebar_layouts WHERE user_email = $1`,
		normalizeEmail(userEmail),
	).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSidebarLayoutNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read sidebar layout: %w", err)
	}
	return raw, nil
}

// SetSidebarLayout saves (replaces) the user's layout JSON.
func (s *Store) SetSidebarLayout(ctx context.Context, userEmail string, layout []byte) error {
	email := normalizeEmail(userEmail)
	if email == "" {
		return errors.New("sidebar layout: user_email is required")
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO chat.sidebar_layouts (user_email, layout, updated_at)
		 VALUES ($1, $2::jsonb, $3)
		 ON CONFLICT (user_email) DO UPDATE
		   SET layout = EXCLUDED.layout, updated_at = EXCLUDED.updated_at`,
		email, string(layout), time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("save sidebar layout: %w", err)
	}
	return nil
}

// DeleteSidebarLayout removes the user's layout (they fall back to the
// workspace default). Deleting an absent layout is not an error.
func (s *Store) DeleteSidebarLayout(ctx context.Context, userEmail string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM chat.sidebar_layouts WHERE user_email = $1`, normalizeEmail(userEmail),
	); err != nil {
		return fmt.Errorf("delete sidebar layout: %w", err)
	}
	return nil
}

// WorkspaceSidebarLayout returns the workspace default layout, or
// ErrSidebarLayoutNotFound.
func (s *Store) WorkspaceSidebarLayout(ctx context.Context) (WorkspaceSidebarLayout, error) {
	var w WorkspaceSidebarLayout
	err := s.db.QueryRowContext(ctx,
		`SELECT layout, updated_by, updated_at FROM chat.workspace_sidebar_layout WHERE id = 1`,
	).Scan(&w.Layout, &w.UpdatedBy, &w.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkspaceSidebarLayout{}, ErrSidebarLayoutNotFound
	}
	if err != nil {
		return WorkspaceSidebarLayout{}, fmt.Errorf("read workspace sidebar layout: %w", err)
	}
	return w, nil
}

// SetWorkspaceSidebarLayout saves (replaces) the workspace default layout and
// returns the stored row.
func (s *Store) SetWorkspaceSidebarLayout(ctx context.Context, layout []byte, updatedBy string) (WorkspaceSidebarLayout, error) {
	w := WorkspaceSidebarLayout{Layout: layout, UpdatedBy: normalizeEmail(updatedBy), UpdatedAt: time.Now().Unix()}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO chat.workspace_sidebar_layout (id, layout, updated_by, updated_at)
		 VALUES (1, $1::jsonb, $2, $3)
		 ON CONFLICT (id) DO UPDATE
		   SET layout = EXCLUDED.layout, updated_by = EXCLUDED.updated_by, updated_at = EXCLUDED.updated_at`,
		string(layout), w.UpdatedBy, w.UpdatedAt,
	); err != nil {
		return WorkspaceSidebarLayout{}, fmt.Errorf("save workspace sidebar layout: %w", err)
	}
	return w, nil
}

// DeleteWorkspaceSidebarLayout removes the workspace default (everyone
// without a layout of their own falls back to the client's built-in one).
// Deleting an absent default is not an error.
func (s *Store) DeleteWorkspaceSidebarLayout(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM chat.workspace_sidebar_layout WHERE id = 1`); err != nil {
		return fmt.Errorf("delete workspace sidebar layout: %w", err)
	}
	return nil
}
