// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// ExternalTeamRename is one team rename central Auth sent: an app-wide
// command, ordered by Version per issuer (docs/CENTRAL-AUTH-PROVISIONING.md,
// "Team renames").
type ExternalTeamRename struct {
	Issuer  string
	Version int64
	EventID string
	From    string
	To      string
}

// ApplyExternalTeamRename applies an Auth team rename exactly once, with the
// same share-keeping rename an admin's Rename team does, and records its
// version in the same transaction. A version at or below the last applied
// one is a no-op (a redelivery after a lost acknowledgement); Auth sends
// renames in order and waits for each, so a newer one never overtakes an
// older one. A rename of a team nothing here is in is applied as a no-op and
// still recorded. applied is false for a skipped version; users is the
// number of accounts relabelled.
//
// Lock order: the per-issuer rename lock, then the two team names
// (renameTeamTx), then rows. The per-account provisioning path never takes
// the rename lock, so the two cannot wait on each other in a cycle.
func (s *Store) ApplyExternalTeamRename(ctx context.Context, r ExternalTeamRename, now int64) (applied bool, users int64, err error) {
	r.Issuer = strings.TrimRight(strings.TrimSpace(r.Issuer), "/")
	if r.Issuer == "" || r.Version <= 0 || strings.TrimSpace(r.EventID) == "" ||
		!ValidExternalTeam(r.From) || !ValidExternalTeam(r.To) || r.From == "" || r.To == "" {
		return false, 0, errInput("invalid team rename")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, 0, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	// Serializes this issuer's renames before the version is read, including
	// the first one, when there is no row to lock yet.
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('external_team_renames'), hashtext($1))`, r.Issuer); err != nil {
		return false, 0, err
	}
	var last int64
	switch scanErr := tx.QueryRowContext(ctx, `SELECT version FROM external_team_renames WHERE issuer = $1`, r.Issuer).Scan(&last); {
	case errors.Is(scanErr, sql.ErrNoRows):
	case scanErr != nil:
		err = scanErr
		return false, 0, err
	}
	if r.Version <= last {
		err = tx.Commit()
		return false, 0, err
	}
	if r.From != r.To {
		if users, _, err = renameTeamTx(ctx, tx, r.From, r.To, now); err != nil {
			return false, 0, err
		}
	}
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO external_team_renames(issuer, version, event_id, from_team, to_team, applied_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (issuer) DO UPDATE SET version = excluded.version, event_id = excluded.event_id,
		  from_team = excluded.from_team, to_team = excluded.to_team, applied_at = excluded.applied_at
		WHERE external_team_renames.version < excluded.version`,
		r.Issuer, r.Version, r.EventID, r.From, r.To, now); err != nil {
		return false, 0, err
	}
	err = tx.Commit()
	return err == nil, users, err
}
