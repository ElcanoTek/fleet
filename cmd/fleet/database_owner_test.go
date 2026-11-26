package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
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

// fakeOwnership is a claim a test loses at will.
type fakeOwnership struct {
	lost chan struct{}
	once sync.Once
}

func newFakeOwnership() *fakeOwnership { return &fakeOwnership{lost: make(chan struct{})} }

func (f *fakeOwnership) lose() { f.once.Do(func() { close(f.lost) }) }

func (f *fakeOwnership) Lost() <-chan struct{} { return f.lost }

func (f *fakeOwnership) Err() error {
	select {
	case <-f.lost:
		return &store.DatabaseOwnedError{Holder: "fleet serve replacement"}
	default:
		return nil
	}
}

// Once the claim is lost during the boot — here right after recovery — the
// boot's context ends, no further step that writes runs, the boot's answer is
// the loss, and the listener-phase watch reports it although its context
// ended with it.
func TestBootStopsWritingTheMomentOwnershipIsLost(t *testing.T) {
	t.Parallel()
	owner := newFakeOwnership()
	ownedCtx, stop := ownedContext(owner)
	defer stop()

	ran := 0
	if err := asOwner(owner, func() error { ran++; return nil }); err != nil || ran != 1 {
		t.Fatalf("a step while the claim holds: ran %d, err %v", ran, err)
	}
	if ownedCtx.Err() != nil {
		t.Fatal("the boot context ended while the claim holds")
	}

	owner.lose()
	select {
	case <-ownedCtx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the boot context did not end when the claim was lost")
	}
	err := asOwner(owner, func() error { ran++; return nil })
	if ran != 1 {
		t.Fatal("a boot step ran after the claim was lost")
	}
	var lost ownershipLostError
	if !errors.As(err, &lost) || !errors.Is(err, store.ErrDatabaseOwned) || exitQuietlyIfStopped(err) == nil {
		t.Fatalf("boot answer after the loss: %v; want the ownership loss, exiting non-zero", err)
	}
	if err := ownershipLossErr(owner); !errors.As(err, &lost) {
		t.Fatalf("the gate before the scheduler starts: %v", err)
	}
	// Steps that write without returning an error skip on the ended context.
	pruneOrphanedSandboxes(ownedCtx, nil)
	bindUserKeysAtBoot(ownedCtx, nil, nil)
	syncSharedFilesAtBoot(ownedCtx, nil)
	if dir := stageSkillsAsOwner(owner, nil, &clientconfig.Bundle{SkillsDir: "bundle-skills"}); dir != "bundle-skills" {
		t.Fatalf("skills staged after the loss: %q", dir)
	}

	// The listener-phase watch, on a context derived from the boot's (ended
	// by the same loss), still reports it.
	for i := 0; i < 50; i++ {
		ctx, cancel := context.WithCancel(ownedCtx)
		errCh := make(chan error, 1)
		stopOnOwnershipLoss(ctx, owner, errCh)
		select {
		case err := <-errCh:
			if !errors.As(err, &lost) {
				t.Fatalf("watch reported %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the loss was dropped by the listener-phase watch")
		}
		cancel()
	}
}

// countingStopper counts Stop calls; a second call would panic like the
// scheduler's (it closes a channel).
type countingStopper struct {
	mu sync.Mutex
	n  int
}

func (c *countingStopper) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	if c.n > 1 {
		panic("stopped twice")
	}
}

func (c *countingStopper) stops() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// The scheduler is stopped the moment the claim is lost, not after the
// shutdown; the deferred stop then does nothing more. Without a loss it is
// stopped only by the deferred stop.
func TestSchedulerStopsWhenOwnershipIsLost(t *testing.T) {
	t.Parallel()
	owner := newFakeOwnership()
	ownedCtx, endOwned := ownedContext(owner)
	defer endOwned()
	sch := &countingStopper{}
	stop := stopSchedulerOnOwnershipLoss(ownedCtx, owner, sch)
	time.Sleep(100 * time.Millisecond)
	if sch.stops() != 0 {
		t.Fatal("the scheduler stopped while the claim holds")
	}
	owner.lose()
	deadline := time.Now().Add(5 * time.Second)
	for sch.stops() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the scheduler did not stop when the claim was lost")
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop() // run()'s deferred stop
	if sch.stops() != 1 {
		t.Fatalf("scheduler stopped %d times", sch.stops())
	}

	// No loss: run() ends (ownedCtx with it), only the deferred stop stops it.
	quiet := newFakeOwnership()
	quietCtx, endQuiet := ownedContext(quiet)
	other := &countingStopper{}
	stopOther := stopSchedulerOnOwnershipLoss(quietCtx, quiet, other)
	stopOther()
	endQuiet()
	time.Sleep(100 * time.Millisecond)
	if other.stops() != 1 {
		t.Fatalf("scheduler stopped %d times without a loss", other.stops())
	}
}

// A watch whose context ends without a loss (a normal shutdown) reports
// nothing.
func TestOwnershipWatchQuietOnShutdown(t *testing.T) {
	t.Parallel()
	owner := newFakeOwnership()
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	stopOnOwnershipLoss(ctx, owner, errCh)
	cancel()
	select {
	case err := <-errCh:
		t.Fatalf("watch reported %v on a normal shutdown", err)
	case <-time.After(300 * time.Millisecond):
	}
}

// run() gates every boot step that writes, from the claim to the listeners,
// on the claim: background writers run under ownedCtx, the steps that return
// an error under asOwner, and the scheduler starts only after one more look
// and is stopped on a loss. No other root context is made in run() after the
// claim but the panic-event writer's (its own bounded write of a recovered
// panic) — a textual check of run() only: helpers it calls may make their
// own, and are covered by the gates above or named in ADR-0122.
func TestRunGatesBootWritesOnTheClaim(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	start := strings.Index(text, "\nfunc run(")
	end := strings.Index(text[start+1:], "\n}\n")
	body := text[start : start+1+end]
	claim := strings.Index(body, "ownedCtx, stopOwned := ownedContext(owner)")
	take := strings.Index(body, "takeTheDatabase(")
	if take < 0 || claim < take {
		t.Fatal("run() must derive ownedCtx right after takeTheDatabase")
	}
	after := body[claim:]
	for _, gated := range []string{
		"asOwner(owner, func() error { return initSchedStorage(",
		"asOwner(owner, func() error { return seedBootstrapAdmins(",
		"startSearchBackfill(ownedCtx,",
		"bindUserKeysAtBoot(ownedCtx,",
		"syncSharedFilesAtBoot(ownedCtx,",
		"stageSkillsAsOwner(owner,",
		"defer stopSchedulerOnOwnershipLoss(ownedCtx, owner, sch)()",
		"pruneOrphanedSandboxes(ownedCtx,",
		"ctx, cancel := context.WithCancel(ownedCtx)",
		"stopOnOwnershipLoss(ctx, owner, errCh)",
	} {
		if !strings.Contains(after, gated) {
			t.Errorf("run() does not gate on the claim: %q", gated)
		}
	}
	gate := strings.Index(after, "if err := ownershipLossErr(owner); err != nil {\n\t\treturn err\n\t}\n\tsch.SetChatScheduleDeliverer(chatSrv)")
	if gate < 0 || gate < strings.Index(after, "recoverAsOwner(") {
		t.Error("run() must look at the claim once more right before the scheduler starts")
	}
	writer := strings.Index(after, "safe.StructuredPanicEventWriter = func(")
	root := strings.Index(after, "context.Background()")
	if n := strings.Count(after, "context.Background()"); n != 1 || writer < 0 || root < writer {
		t.Errorf("run() makes %d root contexts after the claim; only the panic-event writer may", n)
	}
}
