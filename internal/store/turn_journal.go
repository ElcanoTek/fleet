package store

// Durable interactive turn journal + canonical projection (#798).
//
// The SSE turn_events ledger is a delivery/view layer: its tool.result
// payloads are 4 KB UI previews and its writes are async. This file owns the
// durable side of the causal chain
//
//	user input -> tool-call intent -> tool outcome -> assistant conclusion
//
// so a crash (or a failed terminal history write) can never leave external
// side effects without a durable record the next model turn will see:
//
//   - CommitUserMessage persists the user entry BEFORE the first provider
//     call (turn_seq 1).
//   - InsertTurnJournal persists each tool-call intent before dispatch and
//     each governed model-visible result before the next provider step.
//   - CommitTurnHistory transactionally projects the completed transcript
//     into messages and stamps turns.history_committed_at — terminal success
//     is not authoritative before this commit.
//   - RecoverStrandedTurns projects the safe prefix of a crashed turn's
//     journal + full-fidelity turn_events into ONE explicit interrupted turn
//     with provider-valid call/result pairing.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// TurnJournalKind enumerates the two journal record kinds.
const (
	TurnJournalIntent = "tool_intent"
	TurnJournalResult = "tool_result"
)

// TurnJournalRow is one durable journal record: a pre-dispatch tool-call
// intent or a post-governance model-visible result.
type TurnJournalRow struct {
	TurnID      string
	Seq         int64
	Kind        string // TurnJournalIntent | TurnJournalResult
	CallID      string
	ToolName    string
	Content     string // intent: raw input JSON; result: governed model-visible text
	IsErr       bool
	Synthesized bool // recovery-written unknown-outcome results
	CreatedAt   int64
}

// InsertTurnJournal appends one journal row. ON CONFLICT DO NOTHING: the
// writer retries a row verbatim after an AMBIGUOUS failure (a write timeout
// whose INSERT actually landed), and that retry must be absorbed as success —
// without it, the retry's unique-key collision would latch the degraded flag
// and fail the turn precisely because a journal write SUCCEEDED. A genuine
// double-write bug still cannot corrupt anything: the (turn_id, kind,
// call_id) index keeps exactly one record per call either way.
func (s *Store) InsertTurnJournal(ctx context.Context, r TurnJournalRow) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO turn_journal
		   (turn_id, seq, kind, call_id, tool_name, content, is_err, synthesized, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 ON CONFLICT DO NOTHING`,
		r.TurnID, r.Seq, r.Kind, r.CallID, r.ToolName, r.Content, r.IsErr, r.Synthesized, r.CreatedAt,
	)
	return err
}

// LoadTurnJournal returns every journal row for turnID in seq order.
func (s *Store) LoadTurnJournal(ctx context.Context, turnID string) ([]TurnJournalRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT turn_id, seq, kind, call_id, tool_name, content, is_err, synthesized, created_at
		   FROM turn_journal WHERE turn_id = $1 ORDER BY seq`, turnID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []TurnJournalRow
	for rows.Next() {
		var r TurnJournalRow
		if err := rows.Scan(&r.TurnID, &r.Seq, &r.Kind, &r.CallID, &r.ToolName,
			&r.Content, &r.IsErr, &r.Synthesized, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CommitUserMessage durably persists the turn's user entry with provenance
// (turn_id, turn_seq=1) BEFORE the first provider or tool call, in one
// transaction with its FTS row and the conversation bump. The returned id is
// the messages.id the driver later surfaces in history.persisted.
func (s *Store) CommitUserMessage(ctx context.Context, convID, turnID string, entry agent.HistoryEntry) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	ids, err := s.appendHistoryProvenanceTx(ctx, tx, convID, turnID, 1, []agent.HistoryEntry{entry})
	if err != nil {
		return 0, err
	}
	return ids[0], tx.Commit()
}

// ErrTurnHistoryCommitted reports that CommitTurnHistory found the turn's
// canonical projection already committed — a retry after an ambiguous commit
// outcome, absorbed as success by the caller.
var ErrTurnHistoryCommitted = errors.New("turn history already committed")

// CommitTurnHistory transactionally projects a finished turn's transcript
// (everything AFTER the separately-committed user entry — turn_seq 2..N+1)
// into canonical messages history and stamps turns.history_committed_at.
// Terminal turn status (turn.completed / history.persisted) must not be
// advertised until this commits: a failure here is a visible non-success
// terminal state, never a completed answer that disappears on reload.
func (s *Store) CommitTurnHistory(ctx context.Context, convID, turnID string, entries []agent.HistoryEntry) ([]int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// FOR UPDATE guard: exactly one projector wins; a crash-retry after an
	// ambiguous commit sees history_committed_at set and backs off cleanly.
	var committed sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT history_committed_at FROM turns WHERE turn_id = $1 FOR UPDATE`,
		turnID).Scan(&committed); err != nil {
		return nil, fmt.Errorf("CommitTurnHistory: lock turn %s: %w", turnID, err)
	}
	if committed.Valid {
		return nil, ErrTurnHistoryCommitted
	}

	var ids []int64
	if len(entries) > 0 {
		ids, err = s.appendHistoryProvenanceTx(ctx, tx, convID, turnID, 2, entries)
		if err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE turns SET history_committed_at = $1 WHERE turn_id = $2`,
		time.Now().Unix(), turnID); err != nil {
		return nil, err
	}
	return ids, tx.Commit()
}

// appendHistoryProvenanceTx is appendHistoryTx with projection provenance:
// each entry is written with (turn_id, turn_seq = startSeq+i). The partial
// unique index messages_turn_seq makes double projection a loud constraint
// violation instead of a silent duplicate. Branch copies, imports, and
// post-turn approval resolutions use the provenance-less appendHistoryTx and
// stay NULL — they never collide with this index.
func (s *Store) appendHistoryProvenanceTx(ctx context.Context, tx *sql.Tx, convID, turnID string, startSeq int64, entries []agent.HistoryEntry) ([]int64, error) {
	now := time.Now().Unix()

	var b strings.Builder
	b.WriteString(`INSERT INTO messages (conversation_id, role, type, content, created_at, turn_id, turn_seq) VALUES `)
	args := make([]any, 0, len(entries)*7)
	for i, e := range entries {
		if i > 0 {
			b.WriteString(", ")
		}
		base := i*7 + 1
		fmt.Fprintf(&b, "($%d, $%d, $%d, $%d, $%d, $%d, $%d)", base, base+1, base+2, base+3, base+4, base+5, base+6)
		args = append(args, convID, e.Role, e.Type, string(e.Content), now, turnID, startSeq+int64(i))
	}
	b.WriteString(" RETURNING id")
	ids := make([]int64, 0, len(entries))
	rows, err := tx.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	if len(ids) != len(entries) {
		return nil, fmt.Errorf("turn projection: inserted %d messages but got %d ids", len(entries), len(ids))
	}
	if s.searchEnabled {
		if err := insertSearchContent(ctx, tx, convID, now, entries, ids); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE conversations SET updated_at = $1 WHERE id = $2`, now, convID); err != nil {
		return nil, err
	}
	return ids, nil
}

// Model-visible recovery texts. The unknown-outcome warning is what the next
// model reads BEFORE it can repeat a possibly side-effectful call; the
// interrupted marker explains the (possibly truncated) reply above it.
const (
	recoveredUnknownOutcomeText  = "[fleet] The server restarted while this tool call was in flight; its outcome is UNKNOWN. It may or may not have executed. Do not assume it ran and do not silently retry it if it has side effects - verify the outcome first."
	recoveredNeverDispatchedText = "[fleet] This tool call was refused or blocked before execution and then the server restarted. It did NOT execute; no side effect occurred."
	recoveredInterruptedText     = "[fleet] This turn was interrupted by a server restart before completion. The reply above may be incomplete."
)

// RecoveredTurn reports one turn projected by RecoverStrandedTurns.
type RecoveredTurn struct {
	TurnID         string
	ConversationID string
	Projected      int // messages rows written
	Synthesized    int // unknown-outcome results synthesized for resultless calls
}

// RecoverStrandedTurns runs once at startup, before the HTTP server accepts
// traffic. Every turn still flagged 'running' was mid-flight when the previous
// process died. For each, in ONE transaction per turn: reconstruct the safe
// prefix from the durable journal (authoritative for tool calls/results) plus
// the full-fidelity turn_events (best-effort assistant text/reasoning),
// project it into canonical messages with provenance, pair every tool call
// with a result — synthesizing an explicit unknown-outcome error (and a
// synthesized=TRUE journal marker for reconciliation) when the real result
// never landed — then flip the turn to a terminal 'error' with a synthetic
// turn.error event, exactly one explicit interrupted turn per crash.
//
// Idempotent by construction: the status='running' scan predicate, the
// history_committed_at guard, and the messages_turn_seq unique index make a
// repeated recovery (crash during recovery, double boot) a no-op. The entry
// list is a deterministic function of the two ledgers.
//
// Superseded (and removed) the old MarkRunningTurnsErrored startup path,
// which flipped status but reconstructed nothing — the "answer disappears on
// reload" path this issue closes.
func (s *Store) RecoverStrandedTurns(ctx context.Context) ([]RecoveredTurn, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT turn_id, conversation_id FROM turns WHERE status = 'running' ORDER BY started_at`)
	if err != nil {
		return nil, err
	}
	type stranded struct{ turnID, convID string }
	var todo []stranded
	for rows.Next() {
		var t stranded
		if err := rows.Scan(&t.turnID, &t.convID); err != nil {
			_ = rows.Close()
			return nil, err
		}
		todo = append(todo, t)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	var out []RecoveredTurn
	for _, t := range todo {
		rec, err := s.recoverOneTurn(ctx, t.turnID, t.convID)
		if err != nil {
			return out, fmt.Errorf("recover turn %s: %w", t.turnID, err)
		}
		out = append(out, rec)
	}
	return out, nil
}

func (s *Store) recoverOneTurn(ctx context.Context, turnID, convID string) (RecoveredTurn, error) {
	rec := RecoveredTurn{TurnID: turnID, ConversationID: convID}
	journal, err := s.LoadTurnJournal(ctx, turnID)
	if err != nil {
		return rec, err
	}
	events, err := s.LoadTurnEvents(ctx, turnID, 0)
	if err != nil {
		return rec, err
	}

	entries, synthesized := buildRecoveredEntries(journal, events)
	rec.Synthesized = len(synthesized)

	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return rec, err
	}
	defer func() { _ = tx.Rollback() }()

	// Re-check under lock: another recoverer (or a projector that raced the
	// crash) may have terminated this turn already.
	var status string
	var committed sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT status, history_committed_at FROM turns WHERE turn_id = $1 FOR UPDATE`,
		turnID).Scan(&status, &committed); err != nil {
		return rec, err
	}
	if status != string(TurnStatusRunning) {
		return rec, tx.Commit()
	}
	if committed.Valid {
		// The canonical history landed but the process died before FinishTurn:
		// the answer is whole, only the turn ledger is stale. Without this flip
		// the turn stays 'running' forever — never swept, and a reattaching
		// client hangs on a stream that will never end. Complete it; nothing
		// to project.
		if _, err := tx.ExecContext(ctx,
			`UPDATE turns SET status = 'completed', finished_at = $1 WHERE turn_id = $2 AND status = 'running'`,
			now, turnID); err != nil {
			return rec, err
		}
		if err := appendSyntheticTurnEvent(ctx, tx, turnID, "turn.completed",
			`{"recovered":true,"message":"server restarted after this turn's history was saved"}`, now); err != nil {
			return rec, err
		}
		return rec, tx.Commit()
	}

	// Ordering guard: recovery normally runs at boot before any new traffic,
	// but a recovery that failed on an earlier boot can meet a conversation
	// that has MOVED ON (newer turns after the crash). messages.id is the
	// global replay order, so projecting the stale turn NOW would append its
	// old content after newer exchanges and corrupt the conversation. Flip the
	// turn terminal without projecting; the journal keeps the evidence.
	var newerTurns int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM turns
		  WHERE conversation_id = $1 AND turn_id <> $2
		    AND started_at >= (SELECT started_at FROM turns WHERE turn_id = $2)
		    AND status <> 'running'`,
		convID, turnID).Scan(&newerTurns); err != nil {
		return rec, err
	}
	staleBehindNewerTraffic := newerTurns > 0

	// The user entry commits with turn_seq=1 before RunTurn; a crash before
	// that commit leaves nothing for the model to recover (tool calls cannot
	// have run — the commit precedes them). Project after any existing rows.
	var maxSeq sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT MAX(turn_seq) FROM messages WHERE turn_id = $1`, turnID).Scan(&maxSeq); err != nil {
		return rec, err
	}
	userCommitted := maxSeq.Valid
	if userCommitted && !staleBehindNewerTraffic && len(entries) > 0 {
		ids, err := s.appendHistoryProvenanceTx(ctx, tx, convID, turnID, maxSeq.Int64+1, entries)
		if err != nil {
			return rec, err
		}
		rec.Projected = len(ids)
		// Durable reconciliation markers for the synthesized unknown-outcome
		// results, queryable independently of message text. Written as chunked
		// multi-row INSERTs rather than one statement per marker: a crashed
		// turn can carry as many unknown-outcome calls as it had in flight, and
		// recovery pays a round trip for each.
		if err := insertSynthesizedMarkers(ctx, tx, turnID, synthesized, now); err != nil {
			return rec, err
		}
	}

	// Terminal flip + markers, then the synthetic terminal SSE frame so a
	// reattaching client sees clean EOF (the event stays for pagination even
	// though history is now whole).
	if _, err := tx.ExecContext(ctx,
		`UPDATE turns SET status = 'error', finished_at = $1,
		        history_committed_at = $1
		  WHERE turn_id = $2 AND status = 'running'`,
		now, turnID); err != nil {
		return rec, err
	}
	if err := appendSyntheticTurnEvent(ctx, tx, turnID, "turn.error",
		`{"message":"server restarted mid-turn; partial work was recovered into history"}`, now); err != nil {
		return rec, err
	}
	return rec, tx.Commit()
}

// appendSyntheticTurnEvent writes one recovery-authored terminal SSE frame
// inside the caller's transaction, deriving the pagination columns from the
// owning turn the same way InsertTurnEvents does, so a reattaching client
// sees clean EOF instead of hanging on a stream that will never end.
func appendSyntheticTurnEvent(ctx context.Context, tx *sql.Tx, turnID, name, payload string, now int64) error {
	var maxEventID sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT MAX(event_id) FROM turn_events WHERE turn_id = $1`, turnID).Scan(&maxEventID); err != nil {
		return err
	}
	nextEventID := int64(1)
	if maxEventID.Valid {
		nextEventID = maxEventID.Int64 + 1
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO turn_events
		   (turn_id, conversation_id, turn_index, sequence, event_id, event_name, data_json, created_at)
		 SELECT t.turn_id, t.conversation_id, t.turn_index,
		        (SELECT COALESCE(MAX(te.sequence), 0)
		           FROM turn_events te
		          WHERE te.conversation_id = t.conversation_id) + 1,
		        $2, $3, $4, $5
		   FROM turns t WHERE t.turn_id = $1`,
		turnID, nextEventID, name, payload, now,
	)
	return err
}

// insertSynthesizedMarkers writes the recovery-authored synthesized=TRUE
// journal rows inside the caller's transaction as chunked multi-row INSERTs
// (one round trip per maxBatchRows markers instead of one per marker). The
// ON CONFLICT clause is per-statement, so a partially-recovered turn still
// skips markers a previous attempt already wrote.
func insertSynthesizedMarkers(ctx context.Context, tx *sql.Tx, turnID string, rows []synthesizedResult, now int64) error {
	const cols = 7 // is_err and synthesized are literal TRUE, not parameters
	for start := 0; start < len(rows); start += maxBatchRows {
		end := min(start+maxBatchRows, len(rows))
		chunk := rows[start:end]
		var q strings.Builder
		q.WriteString(`INSERT INTO turn_journal
			   (turn_id, seq, kind, call_id, tool_name, content, is_err, synthesized, created_at) VALUES `)
		args := make([]any, 0, len(chunk)*cols)
		for i, row := range chunk {
			if i > 0 {
				q.WriteString(", ")
			}
			base := i * cols
			fmt.Fprintf(&q, "($%d, $%d, $%d, $%d, $%d, $%d, TRUE, TRUE, $%d)",
				base+1, base+2, base+3, base+4, base+5, base+6, base+7)
			args = append(args, turnID, row.Seq, TurnJournalResult, row.CallID, row.ToolName, row.Content, now)
		}
		q.WriteString(" ON CONFLICT (turn_id, kind, call_id) DO NOTHING")
		if _, err := tx.ExecContext(ctx, q.String(), args...); err != nil {
			return err
		}
	}
	return nil
}

// synthesizedResult carries a recovery-synthesized unknown-outcome result so
// recoverOneTurn can persist its reconciliation marker row.
type synthesizedResult struct {
	Seq      int64
	CallID   string
	ToolName string
	Content  string
}

// buildRecoveredEntries reconstructs the interrupted turn's entry list from
// the durable journal (authoritative for tool intents/results) and the
// full-fidelity SSE events (best-effort assistant text/reasoning). Every tool
// call is paired with a result — real when journaled, synthesized
// unknown-outcome otherwise — so replayHistory never produces provider-invalid
// history (a ToolCallPart with no ToolResultPart) and never silently drops a
// side-effectful call.
func buildRecoveredEntries(journal []TurnJournalRow, events []TurnEvent) ([]agent.HistoryEntry, []synthesizedResult) {
	intents := make(map[string]TurnJournalRow) // call_id -> intent
	results := make(map[string]TurnJournalRow) // call_id -> result
	var intentOrder []string                   // journal seq order
	var maxSeq int64
	for _, r := range journal {
		if r.Seq > maxSeq {
			maxSeq = r.Seq
		}
		switch r.Kind {
		case TurnJournalIntent:
			intents[r.CallID] = r
			intentOrder = append(intentOrder, r.CallID)
		case TurnJournalResult:
			results[r.CallID] = r
		}
	}

	var entries []agent.HistoryEntry
	appendJSON := func(role, typ string, v any) {
		raw, err := json.Marshal(v)
		if err != nil {
			return // marshal of plain structs cannot fail; defensive only
		}
		entries = append(entries, agent.HistoryEntry{Role: role, Type: typ, Content: raw})
	}

	// Walk the SSE ledger for ordering + text. tool.call events interleave the
	// accumulated assistant text at the right position; the journal remains
	// authoritative for the call's input bytes. turn.retry drops text streamed
	// for the abandoned attempt (mirrors streamSink.rollbackTo, best-effort).
	var text strings.Builder
	flushText := func() {
		if t := strings.TrimSpace(text.String()); t != "" {
			appendJSON("assistant", "text", agent.TextContent{Text: t})
		}
		text.Reset()
	}
	seenCall := make(map[string]bool)
	seenSteer := make(map[string]bool)
	emitCall := func(callID, name, input string) {
		flushText()
		appendJSON("assistant", "tool_call", agent.ToolCallContent{ID: callID, Name: name, Input: input})
		seenCall[callID] = true
	}
	var synthesized []synthesizedResult
	emitResult := func(callID, name string) {
		if r, ok := results[callID]; ok {
			appendJSON("tool", "tool_result", agent.ToolResultContent{ID: callID, Name: name, Text: r.Content, IsErr: r.IsErr})
			return
		}
		// No result row. When the journal was ACTIVE for this turn (any rows
		// exist) and this call has no INTENT row either, the intent barrier
		// proves the call never dispatched — it was blocked or refused before
		// execution. Warning the model it "may have executed" would be false
		// and could suppress legitimately needed work.
		text := recoveredUnknownOutcomeText
		if len(journal) > 0 {
			if _, dispatched := intents[callID]; !dispatched {
				text = recoveredNeverDispatchedText
			}
		}
		maxSeq++
		synthesized = append(synthesized, synthesizedResult{Seq: maxSeq, CallID: callID, ToolName: name, Content: text})
		appendJSON("tool", "tool_result", agent.ToolResultContent{ID: callID, Name: name, Text: text, IsErr: true})
	}

	for _, ev := range events {
		switch ev.Name {
		case "text.delta":
			var p struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(ev.Data, &p) == nil {
				text.WriteString(p.Text)
			}
		case "turn.retry":
			text.Reset()
		case "user.message":
			// A steered mid-turn user message (#785) normally becomes durable
			// only with the terminal history commit, so an interrupted turn
			// must re-project it here — recovery stamps history_committed_at,
			// which makes RecoverInputQueue complete the steer's queue row as
			// "durably in history"; without this the instruction silently
			// vanishes (#826). Only steered events project: the turn-start
			// user.message (no steered flag) is committed separately at
			// turn_seq=1 and would duplicate. A resilience rollback re-drive
			// can re-emit the same steer, so dedupe by input_id.
			var p struct {
				Text    string `json:"text"`
				Steered bool   `json:"steered"`
				InputID string `json:"input_id"`
			}
			if json.Unmarshal(ev.Data, &p) != nil || !p.Steered || p.InputID == "" || seenSteer[p.InputID] {
				continue
			}
			seenSteer[p.InputID] = true
			flushText()
			appendJSON("user", "text", agent.TextContent{Text: p.Text})
		case "reasoning.end":
			var p struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(ev.Data, &p) == nil && strings.TrimSpace(p.Text) != "" {
				flushText()
				appendJSON("assistant", "reasoning", agent.ReasoningContent{Text: p.Text})
			}
		case "tool.call":
			var p struct {
				ID    string `json:"id"`
				Name  string `json:"name"`
				Input string `json:"input"`
			}
			if json.Unmarshal(ev.Data, &p) != nil || p.ID == "" {
				continue
			}
			input := p.Input
			name := p.Name
			if intent, ok := intents[p.ID]; ok {
				input = intent.Content // journal is authoritative
				name = intent.ToolName
			}
			emitCall(p.ID, name, input)
			emitResult(p.ID, name)
		}
	}
	// Journal intents whose async tool.call event never persisted: append in
	// journal order after the walked prefix — the side effect may have run, so
	// the record must survive even without its SSE frame.
	for _, callID := range intentOrder {
		if seenCall[callID] {
			continue
		}
		intent := intents[callID]
		emitCall(callID, intent.ToolName, intent.Content)
		emitResult(callID, intent.ToolName)
	}
	flushText()

	if len(entries) > 0 {
		appendJSON("assistant", "text", agent.TextContent{Text: recoveredInterruptedText})
		// A zero-usage cancelled summary so the UI shows the interrupted badge;
		// per-step usage checkpointing is deliberately out of scope (ADR-0039).
		appendJSON("assistant", "turn_summary", agent.TurnSummaryContent{Cancelled: true})
	}
	return entries, synthesized
}
