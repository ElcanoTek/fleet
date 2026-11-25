package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/testdb/fleetdb"
)

// A second `fleet serve` on the database a running one owns refuses before
// its boot recovery, so the running server's turn stays running; once the
// first lets go, the next one starts and recovers what it left behind.
func TestSecondServeLeavesTheRunningServersTurnAlone(t *testing.T) {
	t.Parallel()
	dsn := fleetdb.New(t).ChatDSN()
	open := func() *store.Store {
		st, err := store.Open(dsn, store.PoolConfig{MaxOpenConns: 6, MaxIdleConns: 2})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		return st
	}
	a, b := open(), open()
	ctx := context.Background()

	ownerA, releaseA, err := takeTheDatabase(a, 0)
	if err != nil {
		t.Fatalf("first server: %v", err)
	}
	if err := recoverAsOwner(ownerA, a, 30); err != nil {
		t.Fatal(err)
	}
	defer releaseA()

	// The first server's turn, mid tool call.
	const user = "owner-test@example.com"
	if _, err := a.CreateUser(ctx, user, "seed-password-1"); err != nil {
		t.Fatal(err)
	}
	conv, err := a.CreateConversation(ctx, user, "live turn", "", "m", false)
	if err != nil {
		t.Fatal(err)
	}
	turnID := "live-" + conv.ID
	if err := a.CreateTurn(ctx, turnID, conv.ID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CommitUserMessage(ctx, conv.ID, turnID, agent.HistoryEntry{Role: "user", Type: "text", Content: json.RawMessage(`{"text":"run it"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := a.InsertTurnJournal(ctx, store.TurnJournalRow{TurnID: turnID, Seq: 1, Kind: store.TurnJournalIntent, CallID: "call", ToolName: "bash", Content: `{}`}); err != nil {
		t.Fatal(err)
	}

	_, releaseB, err := takeTheDatabase(b, 1500*time.Millisecond)
	if err == nil {
		releaseB()
	}
	if !errors.Is(err, store.ErrDatabaseOwned) || exitQuietlyIfStopped(err) == nil {
		t.Fatalf("second server: err %v; want ErrDatabaseOwned, exiting non-zero", err)
	}
	if !strings.Contains(err.Error(), "fleet serve") {
		t.Errorf("the refusal does not name the server that owns the database: %v", err)
	}
	if rec, err := a.LookupTurn(ctx, turnID); err != nil || rec == nil || rec.Status != store.TurnStatusRunning {
		t.Fatalf("the running server's turn after a second server's boot: %+v, %v; want it still running", rec, err)
	}
	if history, err := a.LoadHistory(ctx, conv.ID); err != nil || len(history) != 1 {
		t.Fatalf("history after a second server's boot: %d entries, %v; want only the user message", len(history), err)
	}

	// The first server stops (here: lets go with its turn still running, as a
	// crash would leave it); the next one takes the database and recovers it.
	releaseA()
	ownerB, releaseB, err := takeTheDatabase(b, 10*time.Second)
	if err != nil {
		t.Fatalf("server after the first stopped: %v", err)
	}
	defer releaseB()
	if err := recoverAsOwner(ownerB, b, 30); err != nil {
		t.Fatal(err)
	}
	if rec, err := b.LookupTurn(ctx, turnID); err != nil || rec == nil || rec.Status == store.TurnStatusRunning {
		t.Fatalf("the left-behind turn after the next boot: %+v, %v; want it recovered", rec, err)
	}
}
