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
//
// Team is the team the provider sent with this state, or nil when the
// provider does not manage the account's team (an older Auth, a migration
// backfill, team sync switched off): Fleet then leaves users.team_id alone.
type ExternalAccessState struct {
	Issuer, Subject, Email string
	Version                int64
	Allowed                bool
	EventID                string
	ChatRole, OpsRole      string
	Team                   *string
	IssuedAt               int64
}

// ValidExternalTeam reports whether team (already trimmed) is a team label an
// identity provider may set: at most maxTeamNameLen bytes, the bound every
// Fleet team write uses, with no control characters. "" (no team) is valid.
func ValidExternalTeam(team string) bool {
	return team == strings.TrimSpace(team) && ValidateTeamName(team) == nil
}

func scanExternalAccessState(row interface{ Scan(...any) error }, state *ExternalAccessState) error {
	var team sql.NullString
	if err := row.Scan(&state.Issuer, &state.Subject, &state.Email, &state.Version, &state.Allowed, &state.EventID,
		&state.ChatRole, &state.OpsRole, &team, &state.IssuedAt); err != nil {
		return err
	}
	state.Team = nil
	if team.Valid {
		v := team.String
		state.Team = &v
	}
	return nil
}

const externalAccessStateColumns = `issuer, subject, email, version, allowed, event_id, chat_role, ops_role, team, issued_at`

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
	err := scanExternalAccessState(s.db.QueryRowContext(ctx, `SELECT `+externalAccessStateColumns+`
		FROM external_access_state WHERE issuer = $1 AND subject = $2`, strings.TrimSpace(issuer), strings.TrimSpace(subject)), &state)
	if errors.Is(err, sql.ErrNoRows) {
		return state, false, nil
	}
	return state, err == nil, err
}

// ApplyExternalAccess applies a newer Auth version atomically to Fleet's Chat
// membership and its durable desired-state row. The Ops plane is reconciled by
// the HTTP layer after commit and retried idempotently if it fails.
//
// A grant that carries a Team sets users.team_id through the same unsharing
// an admin team change does (unshareOnTeamChangeTx), in this transaction: the
// person leaves their old team's shares exactly as if an admin had moved them.
// A revoke never changes the team, and a grant without a Team leaves it alone.
func (s *Store) ApplyExternalAccess(ctx context.Context, next ExternalAccessState) (ExternalAccessState, bool, error) {
	next.Issuer, next.Subject, next.Email = strings.TrimSpace(next.Issuer), strings.TrimSpace(next.Subject), normalizeEmail(next.Email)
	next.EventID, next.ChatRole, next.OpsRole = strings.TrimSpace(next.EventID), strings.TrimSpace(next.ChatRole), strings.TrimSpace(next.OpsRole)
	if next.Team != nil {
		team := strings.TrimSpace(*next.Team)
		next.Team = &team
	}
	if next.Issuer == "" || len(next.Issuer) > 2048 || next.Subject == "" || len(next.Subject) > 255 ||
		next.Email == "" || len(next.Email) > 254 || next.EventID == "" || len(next.EventID) > 255 ||
		next.Version <= 0 || next.IssuedAt <= 0 || !ValidRole(next.ChatRole) || !validOpsRole(next.OpsRole) ||
		(next.Team != nil && !ValidExternalTeam(*next.Team)) {
		return ExternalAccessState{}, false, errors.New("invalid external access state")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExternalAccessState{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var current ExternalAccessState
	err = scanExternalAccessState(tx.QueryRowContext(ctx, `SELECT `+externalAccessStateColumns+`
		FROM external_access_state WHERE issuer = $1 AND subject = $2 FOR UPDATE`, next.Issuer, next.Subject), &current)
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
			var team any
			if next.Team != nil && *next.Team != "" {
				team = *next.Team
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO users(email, password_hash, role, team_id, enabled, created_at, updated_at)
				VALUES($1, $2, $3, $4, TRUE, $5, $5)`, next.Email, "!central-auth-only!", next.ChatRole, team, now); err != nil {
				return ExternalAccessState{}, false, fmt.Errorf("create centrally provisioned user: %w", err)
			}
		} else if next.Team != nil {
			if err := applyExternalTeamTx(ctx, tx, next.Email, *next.Team, now); err != nil {
				return ExternalAccessState{}, false, err
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
	var storedTeam any
	if next.Team != nil {
		storedTeam = *next.Team
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO external_access_state(
		issuer, subject, email, version, allowed, event_id, chat_role, ops_role, team, issued_at, received_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
	ON CONFLICT(issuer, subject) DO UPDATE SET email=EXCLUDED.email, version=EXCLUDED.version,
		allowed=EXCLUDED.allowed, event_id=EXCLUDED.event_id, chat_role=EXCLUDED.chat_role,
		ops_role=EXCLUDED.ops_role, team=EXCLUDED.team, issued_at=EXCLUDED.issued_at, received_at=EXCLUDED.received_at`,
		next.Issuer, next.Subject, next.Email, next.Version, next.Allowed, next.EventID,
		next.ChatRole, next.OpsRole, storedTeam, next.IssuedAt, now)
	if err != nil {
		return ExternalAccessState{}, false, err
	}
	return next, true, tx.Commit()
}

// applyExternalTeamTx moves email to team ("" = no team) on the provider's
// word, with the unsharing an admin move does. A label equal to the current
// one ignoring case is left exactly as it is: every team gate matches
// users.team_id and projects.team_id exactly, so rewriting only the case would
// silently detach the person from their team's shared projects.
func applyExternalTeamTx(ctx context.Context, tx *sql.Tx, email, team string, now int64) error {
	var cur sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT team_id FROM users WHERE email = $1 FOR UPDATE`, email).Scan(&cur); err != nil {
		return err
	}
	if strings.EqualFold(strings.TrimSpace(cur.String), team) {
		return nil
	}
	if err := unshareOnTeamChangeTx(ctx, tx, email, team); err != nil {
		return err
	}
	var teamArg any
	if team != "" {
		teamArg = team
	}
	_, err := tx.ExecContext(ctx, `UPDATE users SET team_id = $2, updated_at = $3 WHERE email = $1`, email, teamArg, now)
	return err
}
