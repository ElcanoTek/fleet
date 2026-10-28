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
	// TeamRename is the version of the provider's last team rename when it
	// wrote this state (settings "team_rename"), or nil when it sent none. A
	// team written before a rename Fleet has since applied names the team
	// under its old name, so ApplyExternalAccess ignores it (roles still
	// apply): an event that was slow to arrive cannot undo a rename.
	TeamRename *int64
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

// normalizeExternalAccessState trims an incoming state and checks its bounds.
func normalizeExternalAccessState(next ExternalAccessState) (ExternalAccessState, error) {
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
		return ExternalAccessState{}, errors.New("invalid external access state")
	}
	return next, nil
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
	next, err := normalizeExternalAccessState(next)
	if err != nil {
		return ExternalAccessState{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExternalAccessState{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	// A team written before the provider's last applied rename is stale.
	// Checked under the rename lock (shared here, exclusive in
	// ApplyExternalTeamRename), taken first so the order is always rename
	// lock, identity lock, team names, rows.
	if next.Team != nil && next.TeamRename != nil {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock_shared(hashtext('external_team_renames'), hashtext($1))`, next.Issuer); err != nil {
			return ExternalAccessState{}, false, err
		}
		var applied int64
		switch scanErr := tx.QueryRowContext(ctx, `SELECT version FROM external_team_renames WHERE issuer = $1`, next.Issuer).Scan(&applied); {
		case errors.Is(scanErr, sql.ErrNoRows):
		case scanErr != nil:
			return ExternalAccessState{}, false, scanErr
		}
		if *next.TeamRename < applied {
			next.Team = nil
		}
	}
	// Serialize every event for this identity, the FIRST one included. The
	// FOR UPDATE below locks the stored state row, but before an identity's
	// first event commits there is no row to lock: two concurrent first
	// events (say a grant at version 1 and the revoke at version 2) both read
	// "no row", both pass the version check, and whichever committed last
	// won — a revoke could commit first and the older grant after it,
	// leaving a revoked account enabled. A transaction-scoped advisory lock
	// on (issuer, subject) exists whether or not the row does, so the second
	// event waits, then reads the first one's committed row and is judged
	// against its version like any later event. The two-key form keeps this
	// lock space apart from the single-key locks used elsewhere.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('external_access_state'), hashtext($1 || chr(10) || $2))`,
		next.Issuer, next.Subject); err != nil {
		return ExternalAccessState{}, false, err
	}
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
		// A grant that moves the account's team takes the team-name locks
		// before the UPDATE below locks the users row (lock order: team
		// names, then user rows — see lockUserTeamChangeTx).
		if err := lockExternalTeamChangeTx(ctx, tx, next); err != nil {
			return ExternalAccessState{}, false, err
		}
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
				// A new account joining a team name nobody holds forms that
				// team afresh; see retireUnheldTeamNameTx.
				if err := retireUnheldTeamNameTx(ctx, tx, *next.Team, now); err != nil {
					return ExternalAccessState{}, false, err
				}
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
	stored, err := upsertExternalAccessStateTx(ctx, tx, next, now)
	if err != nil {
		return ExternalAccessState{}, false, err
	}
	if !stored {
		// A newer state is already stored: this event is rolled back whole,
		// its users-table changes with it.
		_ = tx.Rollback()
		return s.newerStoredExternalAccessState(ctx, next.Issuer, next.Subject)
	}
	return next, true, tx.Commit()
}

// newerStoredExternalAccessState is ApplyExternalAccess's answer when its
// upsert found a newer stored state: that state, unchanged.
func (s *Store) newerStoredExternalAccessState(ctx context.Context, issuer, subject string) (ExternalAccessState, bool, error) {
	current, ok, err := s.ExternalAccessState(ctx, issuer, subject)
	if err != nil {
		return ExternalAccessState{}, false, err
	}
	if !ok {
		return ExternalAccessState{}, false, errors.New("external access state was not stored")
	}
	return current, false, nil
}

// upsertExternalAccessStateTx records next as the identity's desired state,
// reporting false when a stored state of the same or a newer version already
// exists. The version check lives in the upsert itself (WHERE stored.version <
// incoming): the advisory lock in ApplyExternalAccess is what orders events,
// and this keeps the stored row monotonic even for a future path that writes
// it without the lock.
func upsertExternalAccessStateTx(ctx context.Context, tx *sql.Tx, next ExternalAccessState, now int64) (bool, error) {
	var storedTeam any
	if next.Team != nil {
		storedTeam = *next.Team
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO external_access_state(
		issuer, subject, email, version, allowed, event_id, chat_role, ops_role, team, issued_at, received_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
	ON CONFLICT(issuer, subject) DO UPDATE SET email=EXCLUDED.email, version=EXCLUDED.version,
		allowed=EXCLUDED.allowed, event_id=EXCLUDED.event_id, chat_role=EXCLUDED.chat_role,
		ops_role=EXCLUDED.ops_role, team=EXCLUDED.team, issued_at=EXCLUDED.issued_at, received_at=EXCLUDED.received_at
	WHERE external_access_state.version < EXCLUDED.version`,
		next.Issuer, next.Subject, next.Email, next.Version, next.Allowed, next.EventID,
		next.ChatRole, next.OpsRole, storedTeam, next.IssuedAt, now)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// lockExternalTeamChangeTx is lockUserTeamChangeTx for a grant that carries
// a team; a grant without one changes no team and locks nothing.
func lockExternalTeamChangeTx(ctx context.Context, tx *sql.Tx, next ExternalAccessState) error {
	if next.Team == nil {
		return nil
	}
	return lockUserTeamChangeTx(ctx, tx, next.Email, *next.Team)
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
