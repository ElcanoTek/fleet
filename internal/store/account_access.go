package store

import (
	"context"
	"database/sql"
	"errors"
)

// AccountAccess is the Chat plane's membership view of one account: its role,
// its team ("" = none) and whether Central Auth (or an operator) left it
// enabled.
type AccountAccess struct {
	Email   string
	Role    string
	Team    string
	Enabled bool
}

// AccountAccess returns the role, team and enabled flag for email, including
// centrally disabled rows (GetUser hides those). found=false means no Chat
// account exists.
func (s *Store) AccountAccess(ctx context.Context, email string) (AccountAccess, bool, error) {
	a := AccountAccess{Email: normalizeEmail(email)}
	err := s.db.QueryRowContext(ctx, `SELECT role, COALESCE(team_id, ''), enabled FROM users WHERE email = $1`, a.Email).Scan(&a.Role, &a.Team, &a.Enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return AccountAccess{}, false, nil
	}
	if err != nil {
		return AccountAccess{}, false, err
	}
	return a, true, nil
}

// ListAccountAccess returns every Chat account's membership view, disabled rows
// included, ordered by email (`fleet account-events export`).
func (s *Store) ListAccountAccess(ctx context.Context) ([]AccountAccess, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT email, role, COALESCE(team_id, ''), enabled FROM users ORDER BY email ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccountAccess
	for rows.Next() {
		var a AccountAccess
		if err := rows.Scan(&a.Email, &a.Role, &a.Team, &a.Enabled); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
