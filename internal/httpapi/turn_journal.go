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
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

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
