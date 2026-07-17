package store

// Conversation-owned input queue (#785). Submissions that arrive while a turn
// is running become durable queue rows BEFORE the API acknowledges them
// (mirroring the #798 durable-before-acknowledged discipline), then drain as
// ordinary separate turns; steer rows are additionally offered to the running
// turn's PrepareStep boundary and fall back to a queued turn when the turn
// ends first. The claim path is DB-atomic (FOR UPDATE SKIP LOCKED) so the
// drainer needs no process-level lock of its own.

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Input queue modes and states (mirrors the 042 check constraints).
const (
	InputModeQueued = "queued"
	InputModeSteer  = "steer"

	InputStateQueued    = "queued"
	InputStateRunning   = "running"
	InputStateInjected  = "injected"
	InputStateCompleted = "completed"
	InputStateCancelled = "cancelled"
)

// InputQueueRow is one accepted-while-busy submission.
type InputQueueRow struct {
	ID             string
	ConversationID string
	UserEmail      string
	ClientInputID  string
	Message        string
	Attachments    string // JSON array, opaque to the store
	Mode           string
	State          string
	Position       int64
	TurnID         string
	CreatedAt      int64
	UpdatedAt      int64
}

// EnqueueInput inserts a queued input. Idempotent on (conversation_id,
// client_input_id): a replayed POST returns the existing row with
// created=false instead of duplicating the input.
func (s *Store) EnqueueInput(ctx context.Context, r InputQueueRow) (InputQueueRow, bool, error) {
	now := time.Now().Unix()
	r.CreatedAt, r.UpdatedAt, r.State = now, now, InputStateQueued
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO chat_input_queue
		   (id, conversation_id, user_email, client_input_id, message, attachments, mode, state, position, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8,
		         (SELECT COALESCE(MAX(position), 0) + 1 FROM chat_input_queue WHERE conversation_id = $2),
		         $9, $9)
		 ON CONFLICT (conversation_id, client_input_id) DO NOTHING`,
		r.ID, r.ConversationID, r.UserEmail, r.ClientInputID, r.Message, r.Attachments, r.Mode, r.State, now,
	)
	if err != nil {
		return InputQueueRow{}, false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return r, true, nil
	}
	existing, err := s.getInputByClientID(ctx, r.ConversationID, r.ClientInputID)
	if err != nil {
		return InputQueueRow{}, false, err
	}
	return existing, false, nil
}

func (s *Store) getInputByClientID(ctx context.Context, convID, clientID string) (InputQueueRow, error) {
	row := s.db.QueryRowContext(ctx,
		inputQueueSelect+` WHERE conversation_id = $1 AND client_input_id = $2`, convID, clientID)
	return scanInputRow(row)
}

const inputQueueSelect = `SELECT id, conversation_id, user_email, client_input_id, message, attachments,
       mode, state, position, COALESCE(turn_id, ''), created_at, updated_at
  FROM chat_input_queue`

type rowScanner interface{ Scan(dest ...any) error }

func scanInputRow(row rowScanner) (InputQueueRow, error) {
	var r InputQueueRow
	err := row.Scan(&r.ID, &r.ConversationID, &r.UserEmail, &r.ClientInputID, &r.Message,
		&r.Attachments, &r.Mode, &r.State, &r.Position, &r.TurnID, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// ListQueuedInputs returns the conversation's non-terminal rows in drain
// order — the authoritative snapshot the queue UI and reconnects read.
func (s *Store) ListQueuedInputs(ctx context.Context, userEmail, convID string) ([]InputQueueRow, error) {
	rows, err := s.db.QueryContext(ctx,
		inputQueueSelect+` WHERE conversation_id = $1 AND user_email = $2
		    AND state IN ('queued','running','injected')
		  ORDER BY position`, convID, userEmail)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []InputQueueRow
	for rows.Next() {
		r, err := scanInputRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ClaimNextQueuedInput atomically claims the head of the conversation's
// pending queue for turnID (queued -> running). SKIP LOCKED makes concurrent
// drainers safe without process-level coordination; nil means the queue is
// empty.
func (s *Store) ClaimNextQueuedInput(ctx context.Context, convID, turnID string) (*InputQueueRow, error) {
	row := s.db.QueryRowContext(ctx,
		`UPDATE chat_input_queue SET state = 'running', turn_id = $2, updated_at = $3
		  WHERE id = (SELECT id FROM chat_input_queue
		               WHERE conversation_id = $1 AND state = 'queued'
		               ORDER BY position LIMIT 1
		                 FOR UPDATE SKIP LOCKED)
		 RETURNING id, conversation_id, user_email, client_input_id, message, attachments,
		           mode, state, position, COALESCE(turn_id, ''), created_at, updated_at`,
		convID, turnID, time.Now().Unix())
	r, err := scanInputRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// MarkInputInjected flips a steer row queued -> injected for turnID. Guarded
// on state='queued': zero rows means a remove/cancel won the race and the
// caller must refuse injection (the message is gone, not queued).
func (s *Store) MarkInputInjected(ctx context.Context, id, turnID string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE chat_input_queue SET state = 'injected', turn_id = $2, updated_at = $3
		  WHERE id = $1 AND state = 'queued'`, id, turnID, time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// MarkInputTerminal flips one row to completed/cancelled.
func (s *Store) MarkInputTerminal(ctx context.Context, id, state string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE chat_input_queue SET state = $2, updated_at = $3
		  WHERE id = $1 AND state IN ('queued','running','injected')`,
		id, state, time.Now().Unix())
	return err
}

// CompleteInjectedInputs marks a turn's injected steer rows completed — called
// after the turn's canonical history commit, when the steered text became
// durable exactly once (#798 CommitTurnHistory).
func (s *Store) CompleteInjectedInputs(ctx context.Context, turnID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE chat_input_queue SET state = 'completed', updated_at = $2
		  WHERE turn_id = $1 AND state = 'injected'`, turnID, time.Now().Unix())
	return err
}

// CancelQueuedInputs cancels every still-queued row (the Stop-covers-queue
// contract: /cancel scope=all). Running/injected rows belong to the active
// turn's own lifecycle.
func (s *Store) CancelQueuedInputs(ctx context.Context, userEmail, convID string) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE chat_input_queue SET state = 'cancelled', updated_at = $3
		  WHERE conversation_id = $1 AND user_email = $2 AND state = 'queued'`,
		convID, userEmail, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// RemoveQueuedInput cancels one still-queued row; false when it already ran.
func (s *Store) RemoveQueuedInput(ctx context.Context, userEmail, convID, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE chat_input_queue SET state = 'cancelled', updated_at = $4
		  WHERE id = $1 AND conversation_id = $2 AND user_email = $3 AND state = 'queued'`,
		id, convID, userEmail, time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// PromoteQueuedInput moves one still-queued row to the head of the queue
// (send-now while idle drains it first; while busy the driver additionally
// offers it to the running turn's steer mailbox).
func (s *Store) PromoteQueuedInput(ctx context.Context, userEmail, convID, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE chat_input_queue
		    SET position = (SELECT COALESCE(MIN(position), 1) - 1 FROM chat_input_queue WHERE conversation_id = $2),
		        updated_at = $4
		  WHERE id = $1 AND conversation_id = $2 AND user_email = $3 AND state = 'queued'`,
		id, convID, userEmail, time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// RecoverInputQueue runs at boot, after RecoverStrandedTurns. Rows claimed or
// injected by a process that died resolve against the #798 durable record:
//   - running/injected rows whose turn committed the user entry / history are
//     COMPLETED (their text is durably in canonical history);
//   - the rest return to QUEUED (visible + addressable; deliberately NOT
//     auto-drained at boot — restarting the server must not start unattended
//     LLM spend).
func (s *Store) RecoverInputQueue(ctx context.Context) (requeued, completed int, err error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE chat_input_queue SET state = 'completed', updated_at = $1
		  WHERE state = 'running'
		    AND EXISTS (SELECT 1 FROM messages m WHERE m.turn_id = chat_input_queue.turn_id AND m.turn_seq = 1)`,
		time.Now().Unix())
	if err != nil {
		return 0, 0, err
	}
	n, _ := res.RowsAffected()
	completed += int(n)

	res, err = s.db.ExecContext(ctx,
		`UPDATE chat_input_queue SET state = 'completed', updated_at = $1
		  WHERE state = 'injected'
		    AND EXISTS (SELECT 1 FROM turns t WHERE t.turn_id = chat_input_queue.turn_id AND t.history_committed_at IS NOT NULL)`,
		time.Now().Unix())
	if err != nil {
		return 0, completed, err
	}
	n, _ = res.RowsAffected()
	completed += int(n)

	res, err = s.db.ExecContext(ctx,
		`UPDATE chat_input_queue SET state = 'queued', turn_id = NULL, updated_at = $1
		  WHERE state IN ('running','injected')`,
		time.Now().Unix())
	if err != nil {
		return 0, completed, err
	}
	n, _ = res.RowsAffected()
	return int(n), completed, nil
}
