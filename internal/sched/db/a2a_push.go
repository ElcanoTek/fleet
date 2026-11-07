// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

// A2A per-task push-notification configs (#1279 Phase 2): persistence for the
// a2a_push_configs table (migration 066). A row holds one external caller's
// webhook registration; the caller's secrets (token, Authorization
// credentials) are sealed with internal/secretbox before they touch the
// database, AAD-bound to (task_id, id) so a sealed value cannot be replayed
// onto another row. The cipher is injected by cmd/fleet via
// SetA2APushCipher — storing a config with no cipher configured FAILS CLOSED
// rather than persisting plaintext.

package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/secretbox"
)

// ErrA2APushCipherMissing reports that push configs cannot be stored or read
// because no sealing cipher was configured. Named so the handler can turn it
// into an actionable message pointing at FLEET_MCP_OAUTH_ENCRYPTION_KEY.
var ErrA2APushCipherMissing = errors.New("a2a push configs require the store cipher (set FLEET_MCP_OAUTH_ENCRYPTION_KEY)")

// MaxA2APushConfigsPerTask caps how many push configs one task may hold
// (#2040). The spec lets a caller manage several configs per task under its
// own ids, but every config is one outbound POST per status change, so an
// unbounded set would let one caller fan a single task out into an
// arbitrary number of deliveries per tick. Re-registering an existing id is
// an update and never counts against the cap.
const MaxA2APushConfigsPerTask = 10

// ErrA2APushConfigLimit reports that a task already holds
// MaxA2APushConfigsPerTask configs. Named so the handler maps it to an
// invalid-params refusal rather than an internal error.
var ErrA2APushConfigLimit = fmt.Errorf("a task may hold at most %d push-notification configs; delete one first", MaxA2APushConfigsPerTask)

// a2aPushAADPurpose versions the sealed-blob format; bump only with a
// migration story for existing rows.
const a2aPushAADPurpose = "fleet:a2a-push:v1"

// SetA2APushCipher configures the secretbox cipher that seals push-config
// secrets at rest. Mirrors SetLogArchiveKey: injected once by cmd/fleet
// before serving; nil leaves the feature fail-closed (stores refuse).
func (db *Database) SetA2APushCipher(c *secretbox.Cipher) { db.pushCipher = c }

func (db *Database) a2aPushAAD(taskID uuid.UUID, configID string) []byte {
	return secretbox.AAD(a2aPushAADPurpose, taskID.String(), configID)
}

// sealA2APushSecret seals one secret; empty plaintext stores NULL.
func (db *Database) sealA2APushSecret(plaintext string, taskID uuid.UUID, configID string) ([]byte, error) {
	if plaintext == "" {
		return nil, nil
	}
	if db.pushCipher == nil {
		return nil, ErrA2APushCipherMissing
	}
	return db.pushCipher.Seal([]byte(plaintext), db.a2aPushAAD(taskID, configID))
}

// openA2APushSecret unseals one secret; NULL yields "".
func (db *Database) openA2APushSecret(sealed []byte, taskID uuid.UUID, configID string) (string, error) {
	if len(sealed) == 0 {
		return "", nil
	}
	if db.pushCipher == nil {
		return "", ErrA2APushCipherMissing
	}
	plain, err := db.pushCipher.Open(sealed, db.a2aPushAAD(taskID, configID))
	if err != nil {
		return "", fmt.Errorf("unseal a2a push secret: %w", err)
	}
	return string(plain), nil
}

// UpsertA2APushConfig stores a push config, honoring a client-supplied id —
// the A2A spec lets a caller manage multiple configs per task under its own
// ids, and a repeated create for the same id is an update, not a conflict
// (matching the reference implementation). An empty id is server-minted.
// Secrets are sealed before the INSERT; with no cipher the store fails closed.
// A NEW id on a task already holding MaxA2APushConfigsPerTask configs is
// refused with ErrA2APushConfigLimit; the count and the insert run under a
// per-task advisory lock so concurrent creates cannot overshoot the cap.
func (db *Database) UpsertA2APushConfig(ctx context.Context, cfg models.A2APushConfig) (*models.A2APushConfig, error) {
	if cfg.ID == "" {
		cfg.ID = uuid.NewString()
	}
	tokenSealed, err := db.sealA2APushSecret(cfg.Token, cfg.TaskID, cfg.ID)
	if err != nil {
		return nil, err
	}
	credsSealed, err := db.sealA2APushSecret(cfg.AuthCredentials, cfg.TaskID, cfg.ID)
	if err != nil {
		return nil, err
	}
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := acquireSerializationLockTx(ctx, tx, "a2a-push-configs:"+cfg.TaskID.String()); err != nil {
		return nil, err
	}
	var others int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM a2a_push_configs WHERE task_id = $1 AND id <> $2`,
		cfg.TaskID, cfg.ID).Scan(&others); err != nil {
		return nil, err
	}
	if others >= MaxA2APushConfigsPerTask {
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM a2a_push_configs WHERE task_id = $1 AND id = $2)`,
			cfg.TaskID, cfg.ID).Scan(&exists); err != nil {
			return nil, err
		}
		if !exists {
			return nil, ErrA2APushConfigLimit
		}
	}
	// An upsert RESETS the delivery marker: a re-registered config is a fresh
	// subscription, so the current state gets (re-)announced on the next scan.
	row := tx.QueryRowContext(ctx, `
		INSERT INTO a2a_push_configs (task_id, id, url, token_sealed, auth_scheme, auth_credentials_sealed)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (task_id, id) DO UPDATE SET
			url = EXCLUDED.url,
			token_sealed = EXCLUDED.token_sealed,
			auth_scheme = EXCLUDED.auth_scheme,
			auth_credentials_sealed = EXCLUDED.auth_credentials_sealed,
			last_pushed_status = NULL,
			updated_at = now()
		RETURNING created_at, updated_at`,
		cfg.TaskID, cfg.ID, cfg.URL, tokenSealed, cfg.AuthScheme, credsSealed)
	if err := row.Scan(&cfg.CreatedAt, &cfg.UpdatedAt); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	cfg.LastPushedStatus = nil
	return &cfg, nil
}

// GetA2APushConfig loads one config with its secrets unsealed (memory-only).
// sql.ErrNoRows passes through for the handler's not-found mapping.
func (db *Database) GetA2APushConfig(ctx context.Context, taskID uuid.UUID, configID string) (*models.A2APushConfig, error) {
	cfg := models.A2APushConfig{TaskID: taskID, ID: configID}
	var (
		tokenSealed, credsSealed []byte
		lastPushed               sql.NullString
	)
	err := db.conn.QueryRowContext(ctx, `
		SELECT url, token_sealed, auth_scheme, auth_credentials_sealed, last_pushed_status, created_at, updated_at
		FROM a2a_push_configs WHERE task_id = $1 AND id = $2`,
		taskID, configID).
		Scan(&cfg.URL, &tokenSealed, &cfg.AuthScheme, &credsSealed, &lastPushed, &cfg.CreatedAt, &cfg.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if lastPushed.Valid {
		cfg.LastPushedStatus = &lastPushed.String
	}
	if cfg.Token, err = db.openA2APushSecret(tokenSealed, taskID, configID); err != nil {
		return nil, err
	}
	if cfg.AuthCredentials, err = db.openA2APushSecret(credsSealed, taskID, configID); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ListA2APushConfigs returns every config for the task, oldest first,
// secrets unsealed.
func (db *Database) ListA2APushConfigs(ctx context.Context, taskID uuid.UUID) ([]*models.A2APushConfig, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT id, url, token_sealed, auth_scheme, auth_credentials_sealed, last_pushed_status, created_at, updated_at
		FROM a2a_push_configs WHERE task_id = $1 ORDER BY created_at, id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*models.A2APushConfig{}
	for rows.Next() {
		cfg := models.A2APushConfig{TaskID: taskID}
		var (
			tokenSealed, credsSealed []byte
			lastPushed               sql.NullString
		)
		if err := rows.Scan(&cfg.ID, &cfg.URL, &tokenSealed, &cfg.AuthScheme, &credsSealed, &lastPushed, &cfg.CreatedAt, &cfg.UpdatedAt); err != nil {
			return nil, err
		}
		if lastPushed.Valid {
			cfg.LastPushedStatus = &lastPushed.String
		}
		if cfg.Token, err = db.openA2APushSecret(tokenSealed, taskID, cfg.ID); err != nil {
			return nil, err
		}
		if cfg.AuthCredentials, err = db.openA2APushSecret(credsSealed, taskID, cfg.ID); err != nil {
			return nil, err
		}
		out = append(out, &cfg)
	}
	return out, rows.Err()
}

// DeleteA2APushConfig removes one config. Idempotent by spec §3.1.10: a
// second delete of the same id succeeds with nothing to do.
func (db *Database) DeleteA2APushConfig(ctx context.Context, taskID uuid.UUID, configID string) error {
	_, err := db.conn.ExecContext(ctx,
		`DELETE FROM a2a_push_configs WHERE task_id = $1 AND id = $2`, taskID, configID)
	return err
}

// ListA2APushWork returns the due deliveries: every config whose task now has
// a status different from the config's delivery marker. The task ROW is the
// event source — the same source the A2A streams poll — so a status the scan
// never observes (a fast flap between ticks) collapses into the net change,
// which the A2A spec permits ("duplicate deliveries may occur"; only one
// delivery ATTEMPT per change is a MUST). Bounded to keep one tick's work
// finite; the next tick picks up the remainder.
//
// A row whose secrets no longer unseal (the store key rotated, a corrupt
// blob) is SKIPPED, logged, and marked attempted for its current status
// (#2040): failing the whole scan would stall every other delivery behind
// it forever, and leaving it unmarked would re-scan (and re-log) it every
// tick while it holds a batch slot. It is reconsidered on the task's next
// status change, and the caller recovers it by re-registering the config,
// which reseals its secrets under the current key. A missing cipher is a
// deployment error, not a row error, and still fails the scan.
func (db *Database) ListA2APushWork(ctx context.Context, limit int) ([]models.A2APushWork, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.conn.QueryContext(ctx, `
		SELECT c.task_id, c.id, c.url, c.token_sealed, c.auth_scheme, c.auth_credentials_sealed, t.status
		FROM a2a_push_configs c
		JOIN tasks t ON t.id = c.task_id
		WHERE c.last_pushed_status IS DISTINCT FROM t.status
		ORDER BY c.updated_at
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// unreadableRow keeps the sealed bytes the scan saw, so marking it cannot
	// consume a re-registration that resealed the row in between.
	type unreadableRow struct {
		work                     models.A2APushWork
		tokenSealed, credsSealed []byte
	}
	var out []models.A2APushWork
	var unreadable []unreadableRow
	for rows.Next() {
		var (
			w                        models.A2APushWork
			tokenSealed, credsSealed []byte
			status                   string
		)
		if err := rows.Scan(&w.Config.TaskID, &w.Config.ID, &w.Config.URL, &tokenSealed, &w.Config.AuthScheme, &credsSealed, &status); err != nil {
			return nil, err
		}
		w.Status = models.TaskStatus(status)
		token, terr := db.openA2APushSecret(tokenSealed, w.Config.TaskID, w.Config.ID)
		creds, cerr := db.openA2APushSecret(credsSealed, w.Config.TaskID, w.Config.ID)
		if err := errors.Join(terr, cerr); err != nil {
			if errors.Is(err, ErrA2APushCipherMissing) {
				return nil, err
			}
			// Value-free log line: ids only — never the URL or a secret.
			log.Printf("a2a push: skipping config %q on task %s: its secrets do not unseal (store key changed?); re-register the config to resume deliveries: %v",
				w.Config.ID, w.Config.TaskID, err)
			unreadable = append(unreadable, unreadableRow{
				work:        models.A2APushWork{Config: models.A2APushConfig{TaskID: w.Config.TaskID, ID: w.Config.ID}, Status: w.Status},
				tokenSealed: tokenSealed,
				credsSealed: credsSealed,
			})
			continue
		}
		w.Config.Token, w.Config.AuthCredentials = token, creds
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	_ = rows.Close()
	for _, u := range unreadable {
		if _, err := db.markUnreadableA2APush(ctx, u.work.Config.TaskID, u.work.Config.ID, u.work.Status, u.tokenSealed, u.credsSealed); err != nil {
			log.Printf("a2a push: could not mark unreadable config %q on task %s: %v", u.work.Config.ID, u.work.Config.TaskID, err)
		}
	}
	return out, nil
}

// markUnreadableA2APush marks an unreadable config attempted for status, but
// only while the row still holds the sealed secrets the scan failed to open:
// a re-registration between the scan and the mark resealed the row and reset
// its marker, and that recovered config must stay due. Reports whether it
// marked.
func (db *Database) markUnreadableA2APush(ctx context.Context, taskID uuid.UUID, configID string, status models.TaskStatus, tokenSealed, credsSealed []byte) (bool, error) {
	res, err := db.conn.ExecContext(ctx, `
		UPDATE a2a_push_configs SET last_pushed_status = $3, updated_at = now()
		WHERE task_id = $1 AND id = $2
		  AND token_sealed IS NOT DISTINCT FROM $4
		  AND auth_credentials_sealed IS NOT DISTINCT FROM $5
		  AND last_pushed_status IS DISTINCT FROM $3`,
		taskID, configID, string(status), tokenSealed, credsSealed)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// MarkA2APushAttempted records that a delivery ATTEMPT for the given status
// happened (successful or not — the spec's guarantee is at-least-once
// ATTEMPT, and retrying a dead receiver forever would turn the dispatcher
// into a stuck queue). The conditional WHERE makes concurrent dispatchers
// safe: only one marks, mirroring MarkBudgetSoftAlert's one-winner shape.
func (db *Database) MarkA2APushAttempted(ctx context.Context, taskID uuid.UUID, configID string, status models.TaskStatus) (bool, error) {
	res, err := db.conn.ExecContext(ctx, `
		UPDATE a2a_push_configs SET last_pushed_status = $3, updated_at = now()
		WHERE task_id = $1 AND id = $2 AND last_pushed_status IS DISTINCT FROM $3`,
		taskID, configID, string(status))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
