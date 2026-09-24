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
	// InputModeDirect marks the idempotency record of a submission that
	// started a turn directly (migration 064). It is never a queue item: the
	// listing, drain, sweeps, remove and promote skip it, and recovery
	// settles it instead of re-queueing it.
	InputModeDirect = "direct"

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
	// SubmissionID is the client-minted identity of the submission that
	// created this row (#1592), carried separately from ClientInputID: the
	// latter is the idempotency key and carries the unique index, and a row
	// that reported the key as its identity made /inflight echo a value the
	// client never minted. Empty for a submission that names none.
	SubmissionID string
	Message      string
	Attachments  string // JSON array, opaque to the store
	Mode         string
	State        string
	Position     int64
	TurnID       string
	CreatedAt    int64
	UpdatedAt    int64
	// AcceptedSeq is the row's position in the process-wide acceptance
	// order (#1477): the key the Stop scope=all sweep and the claim-limbo
	// gate compare against the counter value a Stop recorded when it began.
	// Rows written without one (an older binary mid-deploy) read back as 0,
	// inside every Stop's swept set.
	AcceptedSeq int64
}

// AcceptedInputSeq returns the acceptance counter's current value: every row
// accepted so far carries a sequence at or below it, and every row accepted
// from now on carries a greater one. A Stop scope=all reads it (under the
// same lock that arms its sweep) as the boundary of its swept set — a memory
// read, so the Stop handler still does no database work before cancelling
// the active turn.
func (s *Store) AcceptedInputSeq() int64 {
	return s.acceptedInputSeq.Load()
}

// seedAcceptedInputSeq starts the counter above every sequence already in the
// table, so a restart cannot hand a new row a value a Stop in the previous
// process would have swept.
func (s *Store) seedAcceptedInputSeq(ctx context.Context) error {
	var maxSeq int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(accepted_seq), 0) FROM chat_input_queue`).Scan(&maxSeq); err != nil {
		return err
	}
	if cur := s.acceptedInputSeq.Load(); maxSeq > cur {
		s.acceptedInputSeq.CompareAndSwap(cur, maxSeq)
	}
	return nil
}

// EnqueueInput inserts a queued input. Idempotent on (conversation_id,
// client_input_id): a replayed POST returns the existing row with
// created=false instead of duplicating the input.
func (s *Store) EnqueueInput(ctx context.Context, r InputQueueRow) (InputQueueRow, bool, error) {
	r.State = InputStateQueued
	return s.insertInput(ctx, r)
}

// ClaimDirectInput records a directly started turn's idempotency key: a row of
// mode 'direct' in state 'running', inserted before the turn launches. It
// shares the queue's unique (conversation_id, client_input_id) index, so a key
// is accepted exactly once whichever path took it; created=false returns the
// row that already holds the key, which the caller answers instead of running
// the input again.
func (s *Store) ClaimDirectInput(ctx context.Context, r InputQueueRow) (InputQueueRow, bool, error) {
	r.Mode, r.State = InputModeDirect, InputStateRunning
	return s.insertInput(ctx, r)
}

// CancelInputKey records a Stop naming key before any input holds it: a
// cancelled direct row in the key space, so a submission that arrives later
// (still in transit when the Stop landed) finds the key taken and is answered
// "cancelled" instead of running. Unlike the in-memory Stop mark it cannot be
// evicted or expire before the submission lands; it is purged with the other
// terminal rows. If a row already holds the key, that row is returned and
// created is false.
func (s *Store) CancelInputKey(ctx context.Context, r InputQueueRow) (InputQueueRow, bool, error) {
	r.Mode, r.State = InputModeDirect, InputStateCancelled
	if r.Attachments == "" {
		r.Attachments = "[]"
	}
	return s.insertInput(ctx, r)
}

// ReleaseDirectInput resolves a direct claim whose turn never launched —
// callers use it only on paths that abort before the turn runs. An unbound
// claim is dropped, so the key is free for the caller to retry. A claim that
// was bound to its turn before the launch was aborted (the bind committed but
// its acknowledgement was lost) cannot be dropped as unbound; it is settled
// cancelled instead (nothing ran), rather than left 'running' to answer every
// resend "already running".
func (s *Store) ReleaseDirectInput(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM chat_input_queue
		  WHERE id = $1 AND mode = 'direct' AND state = 'running' AND turn_id IS NULL`, id); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE chat_input_queue SET state = 'cancelled', updated_at = $2
		  WHERE id = $1 AND mode = 'direct' AND state = 'running'`, id, time.Now().Unix())
	return err
}

// ClaimTurnPrefix marks the placeholder turn id a drain stamps on a row it
// claims, until BindInputTurn replaces it with the real turn id. It is what
// tells a claimed row whose turn has not launched from one bound to a turn.
const ClaimTurnPrefix = "claim-"

// CancelUnlaunchedInput cancels a claimed input whose turn has not been bound
// yet — a direct claim with no turn id, or a drained row still holding its
// drain placeholder — for a Stop naming its key while the turn is prepared.
// The later bind then finds it no longer running and the launch is refused,
// even if the in-memory Stop mark is gone by then. A row bound to its turn is
// left alone: the turn may have run, and "cancelled" would tell a resend of
// the key that nothing ran. It reports whether it cancelled the row.
func (s *Store) CancelUnlaunchedInput(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE chat_input_queue SET state = 'cancelled', updated_at = $2
		  WHERE id = $1 AND state = 'running'
		    AND ((mode = 'direct' AND turn_id IS NULL) OR turn_id LIKE $3)`,
		id, time.Now().Unix(), ClaimTurnPrefix+"%")
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SettleDirectInput resolves a direct claim when its turn ends: completed when
// the turn's user entry committed (the input ran), otherwise cancelled
// (nothing ran). Never re-queued — the caller saw this turn's outcome, and a
// direct input must not run later unattended.
func (s *Store) SettleDirectInput(ctx context.Context, id, turnID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE chat_input_queue SET
		    state = CASE WHEN EXISTS (SELECT 1 FROM messages m WHERE m.turn_id = $2 AND m.turn_seq = 1)
		                 THEN 'completed' ELSE 'cancelled' END,
		    updated_at = $3
		  WHERE id = $1 AND mode = 'direct' AND state = 'running'`,
		id, turnID, time.Now().Unix())
	return err
}

// insertInput is the shared insert behind EnqueueInput and ClaimDirectInput.
func (s *Store) insertInput(ctx context.Context, r InputQueueRow) (InputQueueRow, bool, error) {
	now := time.Now().Unix()
	r.CreatedAt, r.UpdatedAt = now, now
	// Allocated before the insert, so a Stop that reads the counter after this
	// point counts the row as pre-Stop even if the insert has not committed
	// yet (the launch gate then refuses what the sweep could not see). A
	// replayed input_id burns a value on its DO NOTHING path; gaps are fine.
	r.AcceptedSeq = s.acceptedInputSeq.Add(1)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return InputQueueRow{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	// Position allocation is a read-then-write operation. Serialize it on the
	// owning conversation row so concurrent submissions cannot choose the same
	// MAX(position)+1. The schema's non-terminal partial unique index is the
	// final backstop;
	// this lock avoids turning ordinary concurrency into a constraint error.
	if err := lockInputQueueConversation(ctx, tx, r.ConversationID); err != nil {
		return InputQueueRow{}, false, err
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO chat_input_queue
		   (id, conversation_id, user_email, client_input_id, submission_id, message, attachments, mode, state, position, created_at, updated_at, accepted_seq)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9,
		         (SELECT COALESCE(MAX(position), 0) + 1 FROM chat_input_queue WHERE conversation_id = $2),
		         $10, $10, $11)
		 ON CONFLICT (conversation_id, client_input_id) DO NOTHING`,
		r.ID, r.ConversationID, r.UserEmail, r.ClientInputID, r.SubmissionID, r.Message, r.Attachments, r.Mode, r.State, now, r.AcceptedSeq,
	)
	if err != nil {
		return InputQueueRow{}, false, err
	}
	created := false
	if n, _ := res.RowsAffected(); n == 1 {
		created = true
	}
	// Read the stored row even on a fresh insert so the caller receives the
	// database-assigned position rather than the input struct's zero value.
	stored, err := scanInputRow(tx.QueryRowContext(ctx,
		inputQueueSelect+` WHERE conversation_id = $1 AND client_input_id = $2`,
		r.ConversationID, r.ClientInputID))
	if err != nil {
		return InputQueueRow{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return InputQueueRow{}, false, err
	}
	return stored, created, nil
}

func lockInputQueueConversation(ctx context.Context, tx *sql.Tx, convID string) error {
	var id string
	return tx.QueryRowContext(ctx,
		`SELECT id FROM conversations WHERE id = $1 FOR UPDATE`, convID).Scan(&id)
}

func (s *Store) getInputByClientID(ctx context.Context, convID, clientID string) (InputQueueRow, error) {
	row := s.db.QueryRowContext(ctx,
		inputQueueSelect+` WHERE conversation_id = $1 AND client_input_id = $2`, convID, clientID)
	return scanInputRow(row)
}

// inputQueueColumns is the column list every queue read scans (scanInputRow).
// accepted_seq reads back as 0 for a row written without one (an older binary
// mid-deploy): 0 is at or below every Stop boundary, so such a row is swept by
// any Stop — the pre-#1477 behaviour for it.
const inputQueueColumns = `id, conversation_id, user_email, client_input_id,
       COALESCE(submission_id, ''), message, attachments,
       mode, state, position, COALESCE(turn_id, ''), created_at, updated_at,
       COALESCE(accepted_seq, 0)`

const inputQueueSelect = `SELECT ` + inputQueueColumns + `
  FROM chat_input_queue`

type rowScanner interface{ Scan(dest ...any) error }

func scanInputRow(row rowScanner) (InputQueueRow, error) {
	var r InputQueueRow
	err := row.Scan(&r.ID, &r.ConversationID, &r.UserEmail, &r.ClientInputID, &r.SubmissionID,
		&r.Message, &r.Attachments, &r.Mode, &r.State, &r.Position, &r.TurnID, &r.CreatedAt,
		&r.UpdatedAt, &r.AcceptedSeq)
	return r, err
}

// ListQueuedInputs returns the conversation's non-terminal rows in drain
// order — the authoritative snapshot the queue UI and reconnects read.
func (s *Store) ListQueuedInputs(ctx context.Context, userEmail, convID string) ([]InputQueueRow, error) {
	rows, err := s.db.QueryContext(ctx,
		inputQueueSelect+` WHERE conversation_id = $1 AND user_email = $2
		    AND state IN ('queued','running','injected') AND mode <> 'direct'
		  ORDER BY position, created_at, id`, convID, userEmail)
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

// CountPendingInputs returns how many rows are still waiting to drain
// (state='queued') for one conversation — the admission check behind the
// per-conversation queue depth cap.
func (s *Store) CountPendingInputs(ctx context.Context, convID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM chat_input_queue WHERE conversation_id = $1 AND state = 'queued'`,
		convID).Scan(&n)
	return n, err
}

// ClaimNextQueuedInput atomically claims the head of the conversation's
// pending queue for turnID (queued -> running). SKIP LOCKED makes concurrent
// drainers safe without process-level coordination; nil means the queue is
// empty. A row a Stop by key stamped (stop_requested_at) is never claimed:
// it is cancelled instead, as the Stop that stamped it would have withdrawn it.
func (s *Store) ClaimNextQueuedInput(ctx context.Context, convID, turnID string) (*InputQueueRow, error) {
	// A stamped row left queued (its Stop could not withdraw it and was not
	// retried) is cancelled here rather than skipped for good: nothing would
	// ever launch it, and it would otherwise read as queued on every replay.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE chat_input_queue SET state = 'cancelled', updated_at = $2
		  WHERE conversation_id = $1 AND state = 'queued' AND stop_requested_at IS NOT NULL`,
		convID, time.Now().Unix()); err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx,
		`UPDATE chat_input_queue SET state = 'running', turn_id = $2, updated_at = $3
		  WHERE id = (SELECT id FROM chat_input_queue
		               WHERE conversation_id = $1 AND state = 'queued' AND stop_requested_at IS NULL
		               ORDER BY position, created_at, id LIMIT 1
		                 FOR UPDATE SKIP LOCKED)
		 RETURNING `+inputQueueColumns,
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
// caller must refuse injection (the message is gone, not queued, or a Stop
// by key stamped it).
//
// The flip also stamps injected_seq — the turn journal's max seq at injection
// time (#823). The read is race-free against the journal writer: Acknowledge
// runs at the PrepareStep boundary, after every tool goroutine of the prior
// step settled, so every pre-injection intent is already journaled; any tool
// intent with a higher seq was dispatched after the model saw the steer.
func (s *Store) MarkInputInjected(ctx context.Context, id, turnID string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE chat_input_queue SET state = 'injected', turn_id = $2, updated_at = $3,
		        injected_seq = COALESCE((SELECT MAX(seq) FROM turn_journal WHERE turn_id = $2), 0)
		  WHERE id = $1 AND state = 'queued' AND stop_requested_at IS NULL`, id, turnID, time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// CancelStoppedSteer cancels a steer row whose turn a Stop just cancelled —
// still injected, or already returned to the queue by that turn's settlement
// — and never a row the settlement recorded as completed (the steered text
// committed: it ran). It reports whether it cancelled the row.
func (s *Store) CancelStoppedSteer(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE chat_input_queue SET state = 'cancelled', updated_at = $2
		  WHERE id = $1 AND state IN ('injected','queued')`, id, time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// CancelStoppedDrain cancels a drained row whose turn a Stop just confirmed
// stopped, unless that turn committed the row's user entry — the input ran,
// and its settlement records it completed. Left bound to the stopped turn,
// the row would be returned to the queue by that settlement (nothing
// committed) and a later drain could run the input the Stop was answered
// "stopped" for. A row the settlement already returned to the queue, or a
// drain already re-claimed (its bind is then refused), is cancelled all the
// same. It reports whether it cancelled the row.
func (s *Store) CancelStoppedDrain(ctx context.Context, id, turnID string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE chat_input_queue SET state = 'cancelled', updated_at = $3
		  WHERE id = $1 AND mode <> 'direct'
		    AND (state = 'queued'
		      OR (state = 'running' AND turn_id LIKE $4)
		      OR (state = 'running' AND turn_id = $2
		          AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.turn_id = $2 AND m.turn_seq = 1)))`,
		id, turnID, time.Now().Unix(), ClaimTurnPrefix+"%")
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// MarkInputStopRequested records a Stop by key on the key's pending row
// (queued, running or injected), before the Stop cancels the turn running
// it: turn-end settlement and boot recovery then cancel the row, unless its
// input committed, instead of returning it to the queue — the Stop's
// in-memory record does not survive a restart. A key with no pending row is
// left alone (the Stop takes a free key with a cancelled row instead).
func (s *Store) MarkInputStopRequested(ctx context.Context, convID, clientID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE chat_input_queue SET stop_requested_at = $3
		  WHERE conversation_id = $1 AND client_input_id = $2
		    AND state IN ('queued','running','injected')`,
		convID, clientID, time.Now().Unix())
	return err
}

// MarkInputTerminal flips one row to completed/cancelled.
func (s *Store) MarkInputTerminal(ctx context.Context, id, state string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE chat_input_queue SET state = $2, updated_at = $3
		  WHERE id = $1 AND state IN ('queued','running','injected')`,
		id, state, time.Now().Unix())
	return err
}

// MarkClaimedInputTerminal flips one CLAIMED row (state 'running' under the
// given claim placeholder turn id) to state. It is the guarded form the
// launch path uses for every state write before BindInputTurn stamps the real
// turn id: a write that is retried after an ambiguous error must not touch a
// row another drain has since re-claimed (a different placeholder) or bound
// to a live turn — flipping such a row back to 'queued' would run an
// acknowledged input twice. Zero rows affected is the success case there.
func (s *Store) MarkClaimedInputTerminal(ctx context.Context, id, claimTurnID, state string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE chat_input_queue SET state = $3, updated_at = $4
		  WHERE id = $1 AND state = 'running' AND turn_id = $2`,
		id, claimTurnID, state, time.Now().Unix())
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

// CancelQueuedInputs cancels every still-queued row whose acceptance sequence
// is at or below upTo — the Stop-covers-queue contract: /cancel scope=all
// passes the counter value it read when it began (AcceptedInputSeq), so it
// cancels the rows that existed at that instant and nothing accepted after it
// (#1477); a follow-up submitted a moment after the Stop, while the cancelled
// turn was still finishing, keeps the acknowledgement it was given.
// Running/injected rows belong to the active turn's own lifecycle. The
// claim-limbo gate in httpapi refuses by the same comparison, so the two
// agree on exactly one swept set.
func (s *Store) CancelQueuedInputs(ctx context.Context, userEmail, convID string, upTo int64) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE chat_input_queue SET state = 'cancelled', updated_at = $3
		  WHERE conversation_id = $1 AND user_email = $2 AND state = 'queued'
		    AND COALESCE(accepted_seq, 0) <= $4`,
		convID, userEmail, time.Now().Unix(), upTo)
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	// Promotion uses MIN(position)-1 and therefore shares the same allocator
	// lock as enqueue. This keeps simultaneous send-now requests distinct.
	if err := lockInputQueueConversation(ctx, tx, convID); err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE chat_input_queue
		    SET position = (SELECT COALESCE(MIN(position), 1) - 1 FROM chat_input_queue WHERE conversation_id = $2),
		        updated_at = $4
		  WHERE id = $1 AND conversation_id = $2 AND user_email = $3 AND state = 'queued'`,
		id, convID, userEmail, time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n == 1, nil
}

// PurgeTerminalInputs deletes completed/cancelled queue rows whose terminal
// transition is older than retention. Pending, running, and injected rows are
// never retention candidates: they remain durable until their lifecycle is
// resolved. A non-positive retention disables the purge.
func (s *Store) PurgeTerminalInputs(ctx context.Context, retention time.Duration) (int, error) {
	if retention <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-retention).Unix()
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM chat_input_queue
		  WHERE state IN ('completed','cancelled') AND updated_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// RecoverInputQueue runs at boot, after RecoverStrandedTurns. Rows claimed or
// injected by a process that died resolve against the #798 durable record:
//   - running/injected rows whose turn committed the user entry / history are
//     COMPLETED (their text is durably in canonical history);
//   - injected rows with a tool intent journaled after their injection
//     watermark are CANCELLED (#823) — the model may have acted on the steer
//     and the side effects survived (#820), so re-running it could duplicate
//     them (same predicate as SettleTurnInputs);
//   - direct-turn records (mode 'direct') that did not commit are CANCELLED:
//     a direct input is never re-queued;
//   - rows a Stop by key named (stop_requested_at) that did not commit —
//     queued ones included — are CANCELLED: the Stop was answered, and its
//     in-memory record is gone;
//   - the rest return to QUEUED (visible + addressable; deliberately NOT
//     auto-drained at boot — restarting the server must not start unattended
//     LLM spend).
func (s *Store) RecoverInputQueue(ctx context.Context) (requeued, completed, cancelled int, err error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE chat_input_queue SET state = 'completed', updated_at = $1
		  WHERE state = 'running'
		    AND EXISTS (SELECT 1 FROM messages m WHERE m.turn_id = chat_input_queue.turn_id AND m.turn_seq = 1)`,
		time.Now().Unix())
	if err != nil {
		return 0, 0, 0, err
	}
	n, _ := res.RowsAffected()
	completed += int(n)

	res, err = s.db.ExecContext(ctx,
		`UPDATE chat_input_queue SET state = 'completed', updated_at = $1
		  WHERE state = 'injected'
		    AND EXISTS (SELECT 1 FROM turns t WHERE t.turn_id = chat_input_queue.turn_id AND t.history_committed_at IS NOT NULL)`,
		time.Now().Unix())
	if err != nil {
		return 0, completed, 0, err
	}
	n, _ = res.RowsAffected()
	completed += int(n)

	res, err = s.db.ExecContext(ctx,
		`UPDATE chat_input_queue SET state = 'cancelled', updated_at = $1
		  WHERE state = 'injected'
		    AND EXISTS (SELECT 1 FROM turn_journal j
		                 WHERE j.turn_id = chat_input_queue.turn_id AND j.kind = 'tool_intent'
		                   AND j.seq > COALESCE(chat_input_queue.injected_seq, 0))`,
		time.Now().Unix())
	if err != nil {
		return 0, completed, 0, err
	}
	n, _ = res.RowsAffected()
	cancelled = int(n)

	// A direct turn's idempotency record whose turn died before its user entry
	// committed ran nothing: cancel it — a direct input is never re-queued to
	// run later unattended (the committed ones completed in the first step).
	res, err = s.db.ExecContext(ctx,
		`UPDATE chat_input_queue SET state = 'cancelled', updated_at = $1
		  WHERE state = 'running' AND mode = 'direct'`,
		time.Now().Unix())
	if err != nil {
		return 0, completed, cancelled, err
	}
	n, _ = res.RowsAffected()
	cancelled += int(n)

	// A row a Stop by key named (stop_requested_at) whose input never
	// committed (the committed ones completed above) is cancelled, not
	// re-queued: its Stop was answered, and the in-memory record of it died
	// with the process. A stamped row still queued is cancelled too — the
	// process died before the Stop withdrew it, and a drain never claims a
	// stamped row, so it would otherwise sit queued for good.
	res, err = s.db.ExecContext(ctx,
		`UPDATE chat_input_queue SET state = 'cancelled', updated_at = $1
		  WHERE state IN ('queued','running','injected') AND mode <> 'direct' AND stop_requested_at IS NOT NULL`,
		time.Now().Unix())
	if err != nil {
		return 0, completed, cancelled, err
	}
	n, _ = res.RowsAffected()
	cancelled += int(n)

	res, err = s.db.ExecContext(ctx,
		`UPDATE chat_input_queue SET state = 'queued', turn_id = NULL, injected_seq = NULL, updated_at = $1
		  WHERE state IN ('running','injected') AND mode <> 'direct'`,
		time.Now().Unix())
	if err != nil {
		return 0, completed, cancelled, err
	}
	n, _ = res.RowsAffected()
	return int(n), completed, cancelled, nil
}

// BindInputTurn stamps the REAL turn id on a claimed row once registerTurn
// mints it (the claim used a placeholder). Without this the settle/recovery
// predicates — which check the turn's durable #798 record — can never match,
// and a crash would re-queue (double-run) an already-committed input.
// bound is false when the row is no longer running (a Stop cancelled it after
// the claim): the caller must not launch its turn.
func (s *Store) BindInputTurn(ctx context.Context, id, turnID string) (bound bool, err error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE chat_input_queue SET turn_id = $2, updated_at = $3
		  WHERE id = $1 AND state IN ('running','injected')`,
		id, turnID, time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// LookupInput returns the row for a caller idempotency key in any state, or
// nil. The direct submit path consults it so a retry of an ACCEPTED input_id
// that lands while the conversation is idle cannot run a duplicate turn.
func (s *Store) LookupInput(ctx context.Context, convID, clientID string) (*InputQueueRow, error) {
	row, err := s.getInputByClientID(ctx, convID, clientID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// LookupInputForUser returns the most recent row holding a caller idempotency
// key for this user across all of their conversations, or nil. A first
// submission names no conversation — the server creates it — so its resend
// after a response lost before any header must find the original here, before
// a second conversation would be created.
func (s *Store) LookupInputForUser(ctx context.Context, userEmail, clientID string) (*InputQueueRow, error) {
	row, err := scanInputRow(s.db.QueryRowContext(ctx,
		inputQueueSelect+` WHERE user_email = $1 AND client_input_id = $2
		  ORDER BY created_at DESC, id DESC LIMIT 1`, userEmail, clientID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// SettleTurnInputs reconciles a finished turn's queue rows against the #798
// durable record — the same predicates boot recovery uses, applied at turn
// end so no row waits for a restart:
//   - the drained row (drainedID, if any): completed when the turn's user
//     entry committed (messages turn_seq=1), otherwise back to queued — a
//     202-acknowledged input whose turn failed pre-commit is never lost.
//   - injected steer rows: completed when the turn's history committed
//     (normally already done post-commit); otherwise back to queued ONLY when
//     no tool intent was journaled after the row's injection watermark, else
//     CANCELLED (#823) — the model may have acted on the steer before the
//     turn failed, and #820 preserves those committed side effects, so a
//     blind requeue would re-execute the instruction. Intents (not results)
//     carry the proof: they are journaled pre-dispatch, and a degraded
//     journal refuses dispatch outright, so an unjournaled post-injection
//     side effect cannot exist. A NULL watermark (row injected before
//     migration 044) degrades to the coarse gate: any intent blocks requeue.
//   - either kind that a Stop by key named (stop_requested_at) and that did
//     not commit is CANCELLED rather than re-queued: the Stop was answered.
//
// Returns how many rows went back to queued (so the caller can re-kick) and
// how many injected rows were cancelled (so the caller can surface the drop).
func (s *Store) SettleTurnInputs(ctx context.Context, turnID, drainedID string) (requeued, cancelled int, err error) {
	now := time.Now().Unix()
	if drainedID != "" {
		res, err := s.db.ExecContext(ctx,
			`UPDATE chat_input_queue SET
			    state = CASE WHEN EXISTS (SELECT 1 FROM messages m WHERE m.turn_id = $2 AND m.turn_seq = 1)
			                 THEN 'completed'
			                 WHEN stop_requested_at IS NOT NULL THEN 'cancelled'
			                 ELSE 'queued' END,
			    turn_id = CASE WHEN EXISTS (SELECT 1 FROM messages m WHERE m.turn_id = $2 AND m.turn_seq = 1)
			                     OR stop_requested_at IS NOT NULL
			                   THEN turn_id ELSE NULL END,
			    updated_at = $3
			  WHERE id = $1 AND state = 'running'`,
			drainedID, turnID, now)
		if err != nil {
			return 0, 0, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			var state string
			if err := s.db.QueryRowContext(ctx,
				`SELECT state FROM chat_input_queue WHERE id = $1`, drainedID).Scan(&state); err == nil && state == InputStateQueued {
				requeued++
			}
		}
	}
	// One statement decides each uncommitted steer, cancel or re-queue, so a
	// Stop's stamp that lands during settlement cannot fall between a cancel
	// pass and a re-queue pass: the row is decided on the version it has when
	// this statement locks it (READ COMMITTED re-reads a row a concurrent
	// write changed), and a stamp that lands after it finds a queued row,
	// which a drain never claims (ClaimNextQueuedInput skips stamped rows) and
	// the Stop withdraws.
	rows, err := s.db.QueryContext(ctx,
		`UPDATE chat_input_queue SET
		    state = CASE WHEN stop_requested_at IS NOT NULL OR EXISTS (
		                      SELECT 1 FROM turn_journal j
		                       WHERE j.turn_id = $1 AND j.kind = 'tool_intent'
		                         AND j.seq > COALESCE(chat_input_queue.injected_seq, 0))
		                 THEN 'cancelled' ELSE 'queued' END,
		    turn_id = CASE WHEN stop_requested_at IS NOT NULL OR EXISTS (
		                        SELECT 1 FROM turn_journal j
		                         WHERE j.turn_id = $1 AND j.kind = 'tool_intent'
		                           AND j.seq > COALESCE(chat_input_queue.injected_seq, 0))
		                   THEN turn_id ELSE NULL END,
		    injected_seq = CASE WHEN stop_requested_at IS NOT NULL OR EXISTS (
		                             SELECT 1 FROM turn_journal j
		                              WHERE j.turn_id = $1 AND j.kind = 'tool_intent'
		                                AND j.seq > COALESCE(chat_input_queue.injected_seq, 0))
		                        THEN injected_seq ELSE NULL END,
		    updated_at = $2
		  WHERE turn_id = $1 AND state = 'injected'
		    AND NOT EXISTS (SELECT 1 FROM turns t WHERE t.turn_id = $1 AND t.history_committed_at IS NOT NULL)
		 RETURNING state`,
		turnID, now)
	if err != nil {
		return requeued, 0, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var state string
		if err := rows.Scan(&state); err != nil {
			return requeued, cancelled, err
		}
		if state == InputStateCancelled {
			cancelled++
		} else {
			requeued++
		}
	}
	return requeued, cancelled, rows.Err()
}
