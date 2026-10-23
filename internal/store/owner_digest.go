package store

import (
	"context"
	"time"
)

// ownerDigestRetainDays bounds owner_digest_days: a claim older than this can
// no longer matter (only today's row is ever consulted), so the claim that
// inserts a new day prunes the rest.
const ownerDigestRetainDays = 90

// ClaimOwnerDigestDay claims the daily owner digest for the UTC calendar day
// containing day (migration 083, docs/OWNER-DIGEST.md). It returns true for
// exactly one caller per day across restarts and replicas — the one whose
// INSERT created the row — and false for every later caller, which must not
// send. A claim is taken before sending, so a crash after the claim loses
// that day's digest rather than sending it twice (at most once a day).
func (s *Store) ClaimOwnerDigestDay(ctx context.Context, day time.Time) (bool, error) {
	d := day.UTC().Format(time.DateOnly)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO owner_digest_days (day, claimed_at) VALUES ($1::date, $2) ON CONFLICT (day) DO NOTHING`,
		d, time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n != 1 {
		return false, nil
	}
	// Best-effort hygiene: the claim already stands, so a failed prune is
	// not the caller's problem.
	_, _ = s.db.ExecContext(ctx,
		`DELETE FROM owner_digest_days WHERE day < $1::date - $2::int`, d, ownerDigestRetainDays)
	return true, nil
}
