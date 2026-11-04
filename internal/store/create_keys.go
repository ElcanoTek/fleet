package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// createKeyTTL is how long a conversation-create idempotency key is kept: long
// enough for any client retry, short enough that the table stays small.
const createKeyTTL = 24 * time.Hour

// ConversationForCreateKey returns the ID of the conversation the user already
// created with this idempotency key, or "" when there is none (never used, or
// older than a day).
func (s *Store) ConversationForCreateKey(ctx context.Context, userEmail, key string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT conversation_id FROM conversation_create_keys
		  WHERE user_email = $1 AND client_request_id = $2 AND created_at >= $3`,
		normalizeEmail(userEmail), key, time.Now().Add(-createKeyTTL).Unix(),
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read create key: %w", err)
	}
	return id, nil
}

// RecordCreateKey binds an idempotency key to the conversation a create made.
// It returns the conversation the key is bound to: convID when this call
// recorded it, or another conversation's ID when a concurrent create with the
// same key recorded first (the caller then discards its own). Expired keys
// for the user are pruned on the way.
func (s *Store) RecordCreateKey(ctx context.Context, userEmail, key, convID string) (string, error) {
	user := normalizeEmail(userEmail)
	now := time.Now()
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM conversation_create_keys WHERE user_email = $1 AND created_at < $2`,
		user, now.Add(-createKeyTTL).Unix(),
	); err != nil {
		return "", fmt.Errorf("prune create keys: %w", err)
	}
	var bound string
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO conversation_create_keys (user_email, client_request_id, conversation_id, created_at)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (user_email, client_request_id) DO UPDATE
		   SET client_request_id = conversation_create_keys.client_request_id
		 RETURNING conversation_id`,
		user, key, convID, now.Unix(),
	).Scan(&bound)
	if err != nil {
		return "", fmt.Errorf("record create key: %w", err)
	}
	return bound, nil
}

// ForgetCreateKey drops a key whose conversation no longer exists (deleted,
// or rolled back), so a create retried with it makes a new one.
func (s *Store) ForgetCreateKey(ctx context.Context, userEmail, key string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM conversation_create_keys WHERE user_email = $1 AND client_request_id = $2`,
		normalizeEmail(userEmail), key,
	)
	return err
}
