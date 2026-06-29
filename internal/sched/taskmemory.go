package sched

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Persistent task-scoped memory (#198). A scheduled task that opts into
// "Captain's Log" (instruction_self_improve) gets remember/recall tools backed
// by these methods: cross-run facts keyed by (task_id, key), committed
// immediately (scheduled runs are unattended — there is no human to approve a
// write at 3am). This is runtime STATE in the scheduler DB, not the
// operator-owned client-config bundle, so writing it freely preserves the
// reproducibility invariant (the bundle stays a versioned git artifact).

// ErrTaskMemoryNotFound is returned when a (task_id, key) lookup misses.
var ErrTaskMemoryNotFound = errors.New("task memory not found")

// ErrTaskMemoryKeyInvalid is returned when a key is empty or exceeds the cap.
var ErrTaskMemoryKeyInvalid = errors.New("invalid task memory key: must be non-empty and <= 128 chars")

// ErrTaskMemoryValueTooLarge is returned when a value exceeds the configured cap.
var ErrTaskMemoryValueTooLarge = errors.New("task memory value exceeds the configured size cap")

const maxTaskMemoryKeyLen = 128

// TaskMemory is one persisted (key, value) fact for a task.
type TaskMemory struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// UpsertTaskMemory writes (or overwrites) a single fact for a task.
//
// maxKeys bounds the number of distinct keys per task: when inserting a NEW key
// would breach it, the oldest key by updated_at is evicted first (LRU-style), so
// a long-lived recurring task cannot grow its memory unbounded. Updating an
// existing key never triggers eviction. maxValueBytes (>0) hard-rejects an
// oversized value rather than truncating — the agent gets a clear error it can
// act on. maxKeys/maxValueBytes <= 0 disable the respective cap.
func (s *Store) UpsertTaskMemory(ctx context.Context, taskID uuid.UUID, key, value string, maxKeys, maxValueBytes int) error {
	if key == "" || utf8.RuneCountInString(key) > maxTaskMemoryKeyLen {
		return ErrTaskMemoryKeyInvalid
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("task memory value must be valid UTF-8")
	}
	if maxValueBytes > 0 && len(value) > maxValueBytes {
		return ErrTaskMemoryValueTooLarge
	}

	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Is this an update of an existing key (no eviction) or a new key?
	var exists bool
	if err := tx.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM task_memories WHERE task_id = $1 AND key = $2)", taskID, key).
		Scan(&exists); err != nil {
		return err
	}

	if !exists && maxKeys > 0 {
		// Evict oldest keys until inserting one more stays within the cap.
		var count int
		if err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM task_memories WHERE task_id = $1", taskID).Scan(&count); err != nil {
			return err
		}
		for ; count >= maxKeys; count-- {
			if _, err := tx.ExecContext(ctx, `
				DELETE FROM task_memories
				WHERE id = (
					SELECT id FROM task_memories
					WHERE task_id = $1
					ORDER BY updated_at ASC, key ASC
					LIMIT 1
				)`, taskID); err != nil {
				return err
			}
		}
	}

	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO task_memories (id, task_id, key, value, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $5)
		ON CONFLICT (task_id, key) DO UPDATE SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at`,
		uuid.New(), taskID, key, value, now); err != nil {
		return err
	}
	return tx.Commit()
}

// GetTaskMemory returns one fact's value, or ErrTaskMemoryNotFound.
func (s *Store) GetTaskMemory(ctx context.Context, taskID uuid.UUID, key string) (string, error) {
	var value string
	err := s.conn.QueryRowContext(ctx,
		"SELECT value FROM task_memories WHERE task_id = $1 AND key = $2", taskID, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrTaskMemoryNotFound
	}
	if err != nil {
		return "", err
	}
	return value, nil
}

// ListTaskMemories returns all facts for a task, oldest-updated first (a stable,
// meaningful order for prompt injection — the freshest context reads last).
func (s *Store) ListTaskMemories(ctx context.Context, taskID uuid.UUID) ([]TaskMemory, error) {
	rows, err := s.conn.QueryContext(ctx, `
		SELECT key, value, created_at, updated_at
		FROM task_memories WHERE task_id = $1
		ORDER BY updated_at ASC, key ASC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TaskMemory, 0)
	for rows.Next() {
		var m TaskMemory
		if err := rows.Scan(&m.Key, &m.Value, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CountTaskMemories returns how many keys a task has stored.
func (s *Store) CountTaskMemories(ctx context.Context, taskID uuid.UUID) (int, error) {
	var n int
	err := s.conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM task_memories WHERE task_id = $1", taskID).Scan(&n)
	return n, err
}

// DeleteTaskMemory removes a single key. Returns ErrTaskMemoryNotFound if absent.
func (s *Store) DeleteTaskMemory(ctx context.Context, taskID uuid.UUID, key string) error {
	res, err := s.conn.ExecContext(ctx, "DELETE FROM task_memories WHERE task_id = $1 AND key = $2", taskID, key)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrTaskMemoryNotFound
	}
	return nil
}

// DeleteAllTaskMemories clears a task's memory, returning the number removed.
func (s *Store) DeleteAllTaskMemories(ctx context.Context, taskID uuid.UUID) (int64, error) {
	res, err := s.conn.ExecContext(ctx, "DELETE FROM task_memories WHERE task_id = $1", taskID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
