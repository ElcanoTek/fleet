package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Browser Web Push subscriptions (#292). One row per (user, browser)
// PushSubscription, keyed on the relay endpoint URL. The stored keys are the
// browser's public encryption material (RFC 8291) — they let this server send
// to that one browser and nothing else, so they are not treated as secrets,
// but they are still never logged. All reads/writes are scoped by user_email
// at the query level except the endpoint-only delete used by the send path's
// expired-subscription cleanup (a 404/410 from the relay is authoritative).

// PushSubscription is a user's stored browser push subscription.
type PushSubscription struct {
	ID           string
	UserEmail    string
	Endpoint     string
	KeysAuth     string
	KeysP256dh   string
	CreatedAt    int64
	LastActiveAt int64
}

// UpsertPushSubscription inserts or refreshes a subscription row keyed on the
// unique endpoint. A re-subscribe from the same browser (same endpoint) takes
// ownership for userEmail and bumps last_active_at — the browser presenting
// the endpoint's keys IS the subscription's owner, so the newest claim wins.
func (s *Store) UpsertPushSubscription(ctx context.Context, userEmail, endpoint, keysAuth, keysP256dh string) error {
	userEmail = normalizeEmail(userEmail)
	if userEmail == "" || endpoint == "" || keysAuth == "" || keysP256dh == "" {
		return errors.New("push subscription requires user, endpoint, and keys")
	}
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO push_subscriptions (id, user_email, endpoint, keys_auth, keys_p256dh, created_at, last_active_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $6)
		 ON CONFLICT (endpoint) DO UPDATE SET
		   user_email = EXCLUDED.user_email,
		   keys_auth = EXCLUDED.keys_auth,
		   keys_p256dh = EXCLUDED.keys_p256dh,
		   last_active_at = EXCLUDED.last_active_at`,
		uuid.NewString(), userEmail, endpoint, keysAuth, keysP256dh, now,
	)
	return err
}

// ListPushSubscriptions returns every stored subscription for userEmail (all
// the browsers the user enabled notifications in), newest first.
func (s *Store) ListPushSubscriptions(ctx context.Context, userEmail string) ([]PushSubscription, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_email, endpoint, keys_auth, keys_p256dh, created_at, last_active_at
		 FROM push_subscriptions WHERE user_email = $1 ORDER BY created_at DESC`,
		normalizeEmail(userEmail),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PushSubscription
	for rows.Next() {
		var p PushSubscription
		if err := rows.Scan(&p.ID, &p.UserEmail, &p.Endpoint, &p.KeysAuth, &p.KeysP256dh, &p.CreatedAt, &p.LastActiveAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// HasUserPushSubscription reports whether userEmail holds the subscription for
// endpoint — the POST /push/status check that lets the settings switch show
// whether THIS browser gets the signed-in user's notifications, rather than
// only whether the browser holds some subscription (which may be another
// account's on a shared browser, or one the server has since dropped).
func (s *Store) HasUserPushSubscription(ctx context.Context, userEmail, endpoint string) (bool, error) {
	var held bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM push_subscriptions WHERE user_email = $1 AND endpoint = $2)`,
		normalizeEmail(userEmail), endpoint,
	).Scan(&held)
	return held, err
}

// RenewPushSubscription moves userEmail's row from oldEndpoint to the
// browser's new subscription (endpoint + keys). A browser must subscribe
// again after the operator rotates the VAPID keys, because a subscription is
// bound to the key it was made with and the push service refuses every send
// signed with another; the row has to follow it. Owner-scoped: it changes
// nothing and reports false unless userEmail holds the row for oldEndpoint, so
// a renewal can never sign up an account that did not turn notifications on
// in this browser. Idempotent: when the old row is already gone but userEmail
// holds the new endpoint (a second tab renewed first), it reports true.
func (s *Store) RenewPushSubscription(ctx context.Context, userEmail, oldEndpoint, endpoint, keysAuth, keysP256dh string) (bool, error) {
	userEmail = normalizeEmail(userEmail)
	if userEmail == "" || oldEndpoint == "" || endpoint == "" || keysAuth == "" || keysP256dh == "" {
		return false, errors.New("push renewal requires user, both endpoints, and keys")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE push_subscriptions
		 SET endpoint = $3, keys_auth = $4, keys_p256dh = $5, last_active_at = $6
		 WHERE user_email = $1 AND endpoint = $2`,
		userEmail, oldEndpoint, endpoint, keysAuth, keysP256dh, time.Now().Unix(),
	)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 1 {
		return n == 1, err
	}
	var held bool
	err = s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM push_subscriptions WHERE user_email = $1 AND endpoint = $2)`,
		userEmail, endpoint,
	).Scan(&held)
	return held, err
}

// DeletePushSubscription removes a subscription by endpoint regardless of
// owner — the send path's cleanup for endpoints the relay reported expired
// (404/410). Idempotent: deleting an absent endpoint is a no-op.
func (s *Store) DeletePushSubscription(ctx context.Context, endpoint string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM push_subscriptions WHERE endpoint = $1`, endpoint)
	return err
}

// DeleteUserPushSubscription removes a subscription by endpoint, scoped to its
// owner — the DELETE /push/unsubscribe path, so one user can never remove
// another's subscription. Idempotent.
func (s *Store) DeleteUserPushSubscription(ctx context.Context, userEmail, endpoint string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM push_subscriptions WHERE user_email = $1 AND endpoint = $2`,
		normalizeEmail(userEmail), endpoint)
	return err
}
