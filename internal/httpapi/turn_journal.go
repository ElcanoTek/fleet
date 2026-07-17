package httpapi

// Driver-side durable turn journal (#798). agentcore calls the TurnJournal
// seam from inside the tool wrappers; this implementation persists each
// record to the chat store with a short bounded retry. After a write is
// exhausted the writer is DEGRADED: every subsequent intent fails (agentcore
// refuses dispatch — no side effect without a durable record) and the
// terminal-commit closure fails the turn's success instead of advertising a
// completed answer whose evidence never persisted.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
)

// journalWriteTimeout bounds one journal INSERT (its own context: the write
// must survive a Stop-cancelled turnCtx — a cancelled turn still owes its
// journal record for whatever already dispatched).
const journalWriteTimeout = 5 * time.Second

// journalWriteAttempts is the bounded retry for one record. Tool calls are
// 100ms+ operations; two quick retries absorb a Postgres hiccup without
// stalling the loop on a real outage.
const journalWriteAttempts = 3

type turnJournalWriter struct {
	store    chatStore
	turnID   string
	mu       sync.Mutex // serializes seq assignment under parallel tool goroutines
	seq      int64
	degraded atomic.Bool
}

func newTurnJournalWriter(st chatStore, turnID string) *turnJournalWriter {
	return &turnJournalWriter{store: st, turnID: turnID}
}

// write assigns the next seq and inserts with bounded retry. Gap-free seq is
// guaranteed by holding mu across assignment AND insert — pairing is by
// call_id, so cross-call interleaving order doesn't matter, only uniqueness.
func (w *turnJournalWriter) write(kind, callID, toolName, content string, isErr bool) error {
	if w.degraded.Load() {
		return fmt.Errorf("turn journal degraded: an earlier journal write failed")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seq++
	row := store.TurnJournalRow{
		TurnID: w.turnID, Seq: w.seq, Kind: kind, CallID: callID,
		ToolName: toolName, Content: content, IsErr: isErr,
		CreatedAt: time.Now().Unix(),
	}
	var err error
	for attempt := 1; attempt <= journalWriteAttempts; attempt++ {
		wctx, cancel := context.WithTimeout(context.Background(), journalWriteTimeout)
		err = w.store.InsertTurnJournal(wctx, row)
		cancel()
		if err == nil {
			return nil
		}
		time.Sleep(time.Duration(attempt) * 100 * time.Millisecond)
	}
	w.degraded.Store(true)
	log.Printf("turn journal degraded (turn=%s kind=%s tool=%s): %v", w.turnID, kind, toolName, err)
	return err
}

// ToolIntent implements agentcore.TurnJournal: durable before dispatch.
func (w *turnJournalWriter) ToolIntent(_ context.Context, callID, toolName, inputJSON string) error {
	return w.write(store.TurnJournalIntent, callID, toolName, inputJSON, false)
}

// ToolOutcome implements agentcore.TurnJournal: the governed model-visible
// bytes, durable before the next provider step. agentcore ignores the error
// (the side effect already happened); the degraded flag does the enforcement.
func (w *turnJournalWriter) ToolOutcome(_ context.Context, callID, toolName, governedText string, isErr bool) error {
	return w.write(store.TurnJournalResult, callID, toolName, governedText, isErr)
}

// Degraded reports whether any journal write was lost. The terminal-commit
// closure consults it: a turn whose journal is incomplete must not advertise
// success (its canonical history could not be trusted as complete evidence).
func (w *turnJournalWriter) Degraded() bool { return w.degraded.Load() }

// turnCommits is the #798 commit pair runTurnAsync hands the engine, plus the
// row ids the commits captured for the history.persisted dbId backfill. Every
// write uses a fresh context — turnCtx is already dead on Stop, and a
// cancelled turn still owes its partial history.
type turnCommits struct {
	store   chatStore
	convID  string
	turnID  string
	journal *turnJournalWriter

	mu     sync.Mutex
	userID int64
	ids    []int64
}

// commitUser durably persists the user entry BEFORE the first provider call
// (TurnInput.CommitUser).
func (c *turnCommits) commitUser(_ context.Context, entry agent.HistoryEntry) error {
	cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := c.store.CommitUserMessage(cctx, c.convID, c.turnID, entry)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.userID = id
	c.mu.Unlock()
	return nil
}

// commitTerminal transactionally projects the turn's transcript before the
// terminal event is advertised (TurnInput.CommitTerminal). A degraded journal
// refuses success outright: the turn's side-effect record is incomplete.
func (c *turnCommits) commitTerminal(entries []agent.HistoryEntry, _ bool) error {
	if c.journal.Degraded() {
		return fmt.Errorf("the turn journal lost a write; refusing to record success for an incompletely journaled turn")
	}
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		var ids []int64
		ids, err = c.store.CommitTurnHistory(cctx, c.convID, c.turnID, entries)
		cancel()
		if err == nil {
			c.mu.Lock()
			c.ids = ids
			c.mu.Unlock()
			return nil
		}
		if errors.Is(err, store.ErrTurnHistoryCommitted) {
			// A retry after an ambiguous commit outcome: the projection landed;
			// only the row ids are unknown (dbId backfill for this turn arrives
			// on reload instead of live — benign).
			return nil
		}
		time.Sleep(time.Duration(attempt) * 200 * time.Millisecond)
	}
	return err
}

// persisted returns the captured row ids (user entry id, terminal ids).
func (c *turnCommits) persisted() (int64, []int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.userID, c.ids
}
