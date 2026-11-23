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
// ErrSidebarLayoutNotFound when they have none (never saved, or reset).
func (s *Store) SidebarLayout(ctx context.Context, userEmail string) ([]byte, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT layout FROM chat.sidebar_layouts WHERE user_email = $1 AND layout <> 'null'::jsonb`,
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

// SetSidebarLayout saves the user's layout and reports whether it was
// applied. rev orders the user's saves: a save whose rev is not newer than
// the stored one is ignored (applied false), so overlapping requests can
// never leave an older layout on top. rev 0 means "unordered" and always
// applies (a client that does not number its saves).
func (s *Store) SetSidebarLayout(ctx context.Context, userEmail string, layout []byte, rev int64) (bool, error) {
	email := normalizeEmail(userEmail)
	if email == "" {
		return false, errors.New("sidebar layout: user_email is required")
	}
	return s.putSidebarLayout(ctx, email, string(layout), rev)
}

// DeleteSidebarLayout resets the user to the default layout and reports
// whether it was applied. With a rev it is ordered like SetSidebarLayout: the
// row keeps the JSON value null and the rev, so an older save that arrives
// later cannot bring the old layout back. rev 0 deletes the row outright.
func (s *Store) DeleteSidebarLayout(ctx context.Context, userEmail string, rev int64) (bool, error) {
	email := normalizeEmail(userEmail)
	if rev == 0 {
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM chat.sidebar_layouts WHERE user_email = $1`, email,
		); err != nil {
			return false, fmt.Errorf("delete sidebar layout: %w", err)
		}
		return true, nil
	}
	if email == "" {
		return false, errors.New("sidebar layout: user_email is required")
	}
	return s.putSidebarLayout(ctx, email, "null", rev)
}

// SidebarLayoutWithRev reads the user's layout and its revision from the
// same row in one statement, so the revision always identifies the layout
// returned with it (a save landing between two reads could otherwise pair an
// older layout with a newer revision). layout is nil when there is none
// (never saved, or reset); rev is 0 when there is no row.
func (s *Store) SidebarLayoutWithRev(ctx context.Context, userEmail string) ([]byte, int64, error) {
	var raw []byte
	var rev int64
	err := s.db.QueryRowContext(ctx,
		`SELECT CASE WHEN layout = 'null'::jsonb THEN NULL ELSE layout END, rev
		   FROM chat.sidebar_layouts WHERE user_email = $1`,
		normalizeEmail(userEmail),
	).Scan(&raw, &rev)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("read sidebar layout: %w", err)
	}
	return raw, rev, nil
}

// SidebarLayoutRev returns the revision of the user's stored layout (0 when
// they have never saved one). The browser numbers its next save after it, so
// revisions come from one server-held sequence rather than from each
// device's own clock.
func (s *Store) SidebarLayoutRev(ctx context.Context, userEmail string) (int64, error) {
	var rev int64
	err := s.db.QueryRowContext(ctx,
		`SELECT rev FROM chat.sidebar_layouts WHERE user_email = $1`,
		normalizeEmail(userEmail),
	).Scan(&rev)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read sidebar layout rev: %w", err)
	}
	return rev, nil
}

func (s *Store) putSidebarLayout(ctx context.Context, email, layout string, rev int64) (bool, error) {
	var n int64
	err := s.withAccountRow(ctx, email, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO chat.sidebar_layouts (user_email, layout, updated_at, rev)
			 VALUES ($1, $2::jsonb, $3, $4)
			 ON CONFLICT (user_email) DO UPDATE
			   SET layout = EXCLUDED.layout, updated_at = EXCLUDED.updated_at, rev = EXCLUDED.rev
			   WHERE EXCLUDED.rev = 0 OR chat.sidebar_layouts.rev < EXCLUDED.rev`,
			email, layout, time.Now().Unix(), rev,
		)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	if errors.Is(err, ErrUserNotFound) {
		return false, err
	}
	if err != nil {
		return false, fmt.Errorf("save sidebar layout: %w", err)
	}
	return n > 0, nil
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
