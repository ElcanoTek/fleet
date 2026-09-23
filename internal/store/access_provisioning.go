package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ExternalAccessState is Fleet's last accepted desired membership from Auth.
// Roles are retained across a revoke so re-grant is non-destructive.
type ExternalAccessState struct {
	Issuer, Subject, Email string
	Version                int64
	Allowed                bool
	EventID                string
	ChatRole, OpsRole      string
	IssuedAt               int64
}

func validOpsRole(role string) bool {
	switch role {
	case "none", "readonly", "client", "admin":
		return true
	default:
		return false
	}
}

func (s *Store) ExternalAccessState(ctx context.Context, issuer, subject string) (ExternalAccessState, bool, error) {
	var state ExternalAccessState
	err := s.db.QueryRowContext(ctx, `SELECT issuer, subject, email, version, allowed, event_id, chat_role, ops_role, issued_at
		FROM external_access_state WHERE issuer = $1 AND subject = $2`, strings.TrimSpace(issuer), strings.TrimSpace(subject)).Scan(
		&state.Issuer, &state.Subject, &state.Email, &state.Version, &state.Allowed, &state.EventID,
		&state.ChatRole, &state.OpsRole, &state.IssuedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return state, false, nil
	}
	return state, err == nil, err
}

// ApplyExternalAccess applies a newer Auth version atomically to Fleet's Chat
// membership and its durable desired-state row. The Ops plane is reconciled by
// the HTTP layer after commit and retried idempotently if it fails.
func (s *Store) ApplyExternalAccess(ctx context.Context, next ExternalAccessState) (ExternalAccessState, bool, error) {
	next.Issuer, next.Subject, next.Email = strings.TrimSpace(next.Issuer), strings.TrimSpace(next.Subject), normalizeEmail(next.Email)
	next.EventID, next.ChatRole, next.OpsRole = strings.TrimSpace(next.EventID), strings.TrimSpace(next.ChatRole), strings.TrimSpace(next.OpsRole)
	if next.Issuer == "" || len(next.Issuer) > 2048 || next.Subject == "" || len(next.Subject) > 255 ||
		next.Email == "" || len(next.Email) > 254 || next.EventID == "" || len(next.EventID) > 255 ||
		next.Version <= 0 || next.IssuedAt <= 0 || !ValidRole(next.ChatRole) || !validOpsRole(next.OpsRole) {
		return ExternalAccessState{}, false, errors.New("invalid external access state")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExternalAccessState{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var current ExternalAccessState
	err = tx.QueryRowContext(ctx, `SELECT issuer, subject, email, version, allowed, event_id, chat_role, ops_role, issued_at
		FROM external_access_state WHERE issuer = $1 AND subject = $2 FOR UPDATE`, next.Issuer, next.Subject).Scan(
		&current.Issuer, &current.Subject, &current.Email, &current.Version, &current.Allowed, &current.EventID,
		&current.ChatRole, &current.OpsRole, &current.IssuedAt)
	if err == nil {
		if current.Email != next.Email {
			return ExternalAccessState{}, false, errors.New("external identity email changed")
		}
		if current.Version >= next.Version {
			return current, false, tx.Commit()
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return ExternalAccessState{}, false, err
	}
	now := time.Now().Unix()
	if next.Allowed {
		res, err := tx.ExecContext(ctx, `UPDATE users SET enabled = TRUE, role = $2, updated_at = $3 WHERE email = $1`, next.Email, next.ChatRole, now)
		if err != nil {
			return ExternalAccessState{}, false, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// This sentinel is deliberately not a valid bcrypt digest. Central
			// users authenticate through OIDC; VerifyUser treats it as a generic
			// bad password if the legacy password form is attempted.
			if _, err := tx.ExecContext(ctx, `INSERT INTO users(email, password_hash, role, enabled, created_at, updated_at)
				VALUES($1, $2, $3, TRUE, $4, $4)`, next.Email, "!central-auth-only!", next.ChatRole, now); err != nil {
				return ExternalAccessState{}, false, fmt.Errorf("create centrally provisioned user: %w", err)
			}
		}
	} else {
		salt, err := newExternalSessionEpoch()
		if err != nil {
			return ExternalAccessState{}, false, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET enabled = FALSE, session_salt = $2, updated_at = $3 WHERE email = $1`, next.Email, salt, now); err != nil {
			return ExternalAccessState{}, false, err
		}
		epoch, err := newExternalSessionEpoch()
		if err != nil {
			return ExternalAccessState{}, false, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO external_auth_epochs(issuer, subject, email, epoch, created_at, updated_at)
			VALUES($1, $2, $3, $4, $5, $5) ON CONFLICT(issuer, subject) DO UPDATE SET
			email = EXCLUDED.email, epoch = EXCLUDED.epoch, updated_at = EXCLUDED.updated_at`,
			next.Issuer, next.Subject, next.Email, epoch, now); err != nil {
			return ExternalAccessState{}, false, err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO external_access_state(
		issuer, subject, email, version, allowed, event_id, chat_role, ops_role, issued_at, received_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
	ON CONFLICT(issuer, subject) DO UPDATE SET email=EXCLUDED.email, version=EXCLUDED.version,
		allowed=EXCLUDED.allowed, event_id=EXCLUDED.event_id, chat_role=EXCLUDED.chat_role,
		ops_role=EXCLUDED.ops_role, issued_at=EXCLUDED.issued_at, received_at=EXCLUDED.received_at`,
		next.Issuer, next.Subject, next.Email, next.Version, next.Allowed, next.EventID,
		next.ChatRole, next.OpsRole, next.IssuedAt, now)
	if err != nil {
		return ExternalAccessState{}, false, err
	}
	return next, true, tx.Commit()
}
