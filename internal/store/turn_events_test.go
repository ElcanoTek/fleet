package store

import (
	"context"
	"testing"
	"time"
)

func TestTurnLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Setup user & conversation to satisfy foreign keys
	if _, err := s.CreateUser(ctx, "turn@example.com", "password123"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	conv, err := s.CreateConversation(ctx, "turn@example.com", "t", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	turnID := "turn_123"

	// 1. Lookup missing turn
	r, err := s.LookupTurn(ctx, turnID)
	if err != nil {
		t.Fatalf("LookupTurn missing err: %v", err)
	}
	if r != nil {
		t.Fatalf("expected nil for missing turn, got %v", r)
	}

	// 2. Create turn
	startedAt := time.Now().Unix()
	if err := s.CreateTurn(ctx, turnID, conv.ID, startedAt); err != nil {
		t.Fatalf("CreateTurn: %v", err)
	}

	r, err = s.LookupTurn(ctx, turnID)
	if err != nil {
		t.Fatalf("LookupTurn after create: %v", err)
	}
	if r == nil {
		t.Fatalf("LookupTurn returned nil for created turn")
	}
	if r.Status != TurnStatusRunning {
		t.Errorf("expected status %s, got %s", TurnStatusRunning, r.Status)
	}
	if r.StartedAt != startedAt {
		t.Errorf("expected startedAt %d, got %d", startedAt, r.StartedAt)
	}

	// 3. Finish turn
	finishedAt := startedAt + 10
	if err := s.FinishTurn(ctx, turnID, TurnStatusCompleted, finishedAt, false); err != nil {
		t.Fatalf("FinishTurn: %v", err)
	}

	r, err = s.LookupTurn(ctx, turnID)
	if err != nil {
		t.Fatalf("LookupTurn after finish: %v", err)
	}
	if r == nil {
		t.Fatalf("LookupTurn returned nil for finished turn")
	}
	if r.Status != TurnStatusCompleted {
		t.Errorf("expected status %s, got %s", TurnStatusCompleted, r.Status)
	}
	if !r.FinishedAt.Valid || r.FinishedAt.Int64 != finishedAt {
		t.Errorf("expected valid finishedAt %d, got valid=%v int64=%d", finishedAt, r.FinishedAt.Valid, r.FinishedAt.Int64)
	}
	// A turn finished without loss is not flagged lossy.
	if r.Lossy {
		t.Errorf("expected lossy=false for a clean turn, got true")
	}

	// 4. The lossy flag round-trips: a turn finished as lossy reads back lossy.
	lossyTurn := "turn_lossy"
	if err := s.CreateTurn(ctx, lossyTurn, conv.ID, startedAt); err != nil {
		t.Fatalf("CreateTurn(lossy): %v", err)
	}
	if err := s.FinishTurn(ctx, lossyTurn, TurnStatusCompleted, finishedAt, true); err != nil {
		t.Fatalf("FinishTurn(lossy): %v", err)
	}
	lr, err := s.LookupTurn(ctx, lossyTurn)
	if err != nil {
		t.Fatalf("LookupTurn(lossy): %v", err)
	}
	if lr == nil || !lr.Lossy {
		t.Errorf("expected lossy=true to round-trip, got %+v", lr)
	}
}

func TestInsertAndLoadTurnEvents(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Setup
	if _, err := s.CreateUser(ctx, "events@example.com", "password123"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	conv, err := s.CreateConversation(ctx, "events@example.com", "t", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	turnID := "turn_events_1"
	if err := s.CreateTurn(ctx, turnID, conv.ID, time.Now().Unix()); err != nil {
		t.Fatalf("CreateTurn: %v", err)
	}

	// 1. Empty slice is a no-op
	if err := s.InsertTurnEvents(ctx, []TurnEvent{}); err != nil {
		t.Fatalf("InsertTurnEvents empty: %v", err)
	}

	// 2. Insert batch
	events := []TurnEvent{
		{TurnID: turnID, EventID: 1, Name: "start", Data: []byte(`{"a":1}`), CreatedAt: 100},
		{TurnID: turnID, EventID: 2, Name: "mid", Data: []byte(`{"a":2}`), CreatedAt: 101},
		{TurnID: turnID, EventID: 3, Name: "end", Data: []byte(`{"a":3}`), CreatedAt: 102},
	}
	if err := s.InsertTurnEvents(ctx, events); err != nil {
		t.Fatalf("InsertTurnEvents: %v", err)
	}

	// 3. Load all events
	loaded, err := s.LoadTurnEvents(ctx, turnID, 0)
	if err != nil {
		t.Fatalf("LoadTurnEvents(0): %v", err)
	}
	if len(loaded) != 3 {
		t.Fatalf("expected 3 events, got %d", len(loaded))
	}
	if loaded[0].EventID != 1 || loaded[2].EventID != 3 {
		t.Errorf("wrong event sequence loaded: %v", loaded)
	}

	// 4. Load events skipping the first
	loadedAfter1, err := s.LoadTurnEvents(ctx, turnID, 1)
	if err != nil {
		t.Fatalf("LoadTurnEvents(1): %v", err)
	}
	if len(loadedAfter1) != 2 {
		t.Fatalf("expected 2 events after ID 1, got %d", len(loadedAfter1))
	}
	if loadedAfter1[0].EventID != 2 {
		t.Errorf("expected first loaded event to be ID 2, got %d", loadedAfter1[0].EventID)
	}

	// 5. Insert duplicates - ON CONFLICT DO NOTHING
	duplicateEvents := []TurnEvent{
		{TurnID: turnID, EventID: 1, Name: "start_override", Data: []byte(`{"a":99}`), CreatedAt: 999},
		{TurnID: turnID, EventID: 4, Name: "new_event", Data: []byte(`{"a":4}`), CreatedAt: 103},
	}
	if err := s.InsertTurnEvents(ctx, duplicateEvents); err != nil {
		t.Fatalf("InsertTurnEvents with duplicates: %v", err)
	}

	// Verify only the new event was added, and the old event 1 wasn't updated
	finalLoaded, err := s.LoadTurnEvents(ctx, turnID, 0)
	if err != nil {
		t.Fatalf("LoadTurnEvents final: %v", err)
	}
	if len(finalLoaded) != 4 {
		t.Fatalf("expected 4 events, got %d", len(finalLoaded))
	}
	if string(finalLoaded[0].Data) != `{"a":1}` {
		t.Errorf("duplicate insert overwrote data! expected `{\"a\":1}`, got `%s`", string(finalLoaded[0].Data))
	}
	if finalLoaded[3].EventID != 4 {
		t.Errorf("expected final event ID 4, got %d", finalLoaded[3].EventID)
	}
}

func TestMarkRunningTurnsErrored(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Setup
	if _, err := s.CreateUser(ctx, "error@example.com", "password123"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	conv, err := s.CreateConversation(ctx, "error@example.com", "t", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	// 1. Running turn
	turnRunning := "turn_running"
	if err := s.CreateTurn(ctx, turnRunning, conv.ID, time.Now().Unix()); err != nil {
		t.Fatalf("CreateTurn: %v", err)
	}
	// Add an event so we can test the next event_id calculation
	if err := s.InsertTurnEvents(ctx, []TurnEvent{{TurnID: turnRunning, EventID: 1, Name: "test", Data: []byte("{}"), CreatedAt: time.Now().Unix()}}); err != nil {
		t.Fatalf("InsertTurnEvents: %v", err)
	}

	// 2. Finished turn
	turnFinished := "turn_finished"
	if err := s.CreateTurn(ctx, turnFinished, conv.ID, time.Now().Unix()); err != nil {
		t.Fatalf("CreateTurn: %v", err)
	}
	if err := s.FinishTurn(ctx, turnFinished, TurnStatusCompleted, time.Now().Unix(), false); err != nil {
		t.Fatalf("FinishTurn: %v", err)
	}

	// Mark running turns errored
	touched, err := s.MarkRunningTurnsErrored(ctx)
	if err != nil {
		t.Fatalf("MarkRunningTurnsErrored: %v", err)
	}

	// Verify only the running turn was touched
	if len(touched) != 1 || touched[0] != turnRunning {
		t.Fatalf("expected touched [%s], got %v", turnRunning, touched)
	}

	// Verify status upgraded to error
	r, _ := s.LookupTurn(ctx, turnRunning)
	if r.Status != TurnStatusError {
		t.Errorf("expected running turn to be upgraded to %s, got %s", TurnStatusError, r.Status)
	}
	if !r.FinishedAt.Valid {
		t.Errorf("expected finished_at to be populated for errored turn")
	}

	// Verify finished turn is untouched
	rFin, _ := s.LookupTurn(ctx, turnFinished)
	if rFin.Status != TurnStatusCompleted {
		t.Errorf("expected finished turn to stay %s, got %s", TurnStatusCompleted, rFin.Status)
	}

	// Verify synthetic event appended
	events, err := s.LoadTurnEvents(ctx, turnRunning, 0)
	if err != nil {
		t.Fatalf("LoadTurnEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	synthEvent := events[1]
	if synthEvent.EventID != 2 {
		t.Errorf("expected synthetic event to have EventID 2, got %d", synthEvent.EventID)
	}
	if synthEvent.Name != "turn.error" {
		t.Errorf("expected synthetic event name 'turn.error', got '%s'", synthEvent.Name)
	}
}

func TestSweepTurnEvents(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Setup
	if _, err := s.CreateUser(ctx, "sweep@example.com", "password123"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	conv, err := s.CreateConversation(ctx, "sweep@example.com", "t", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	twoHoursAgo := time.Now().Add(-2 * time.Hour).Unix()
	halfHourAgo := time.Now().Add(-30 * time.Minute).Unix()

	// 1. Old turn (should be swept)
	turnOld := "turn_old"
	if err := s.CreateTurn(ctx, turnOld, conv.ID, twoHoursAgo-10); err != nil {
		t.Fatalf("CreateTurn: %v", err)
	}
	if err := s.FinishTurn(ctx, turnOld, TurnStatusCompleted, twoHoursAgo, false); err != nil {
		t.Fatalf("FinishTurn: %v", err)
	}

	// 2. Recent turn (should NOT be swept)
	turnRecent := "turn_recent"
	if err := s.CreateTurn(ctx, turnRecent, conv.ID, halfHourAgo-10); err != nil {
		t.Fatalf("CreateTurn: %v", err)
	}
	if err := s.FinishTurn(ctx, turnRecent, TurnStatusCompleted, halfHourAgo, false); err != nil {
		t.Fatalf("FinishTurn: %v", err)
	}

	// 3. Running turn (should NOT be swept, even if started old)
	turnRunning := "turn_running"
	if err := s.CreateTurn(ctx, turnRunning, conv.ID, twoHoursAgo); err != nil {
		t.Fatalf("CreateTurn: %v", err)
	}

	// Sweep with TTL of 1 hour
	deleted, err := s.SweepTurnEvents(ctx, time.Hour)
	if err != nil {
		t.Fatalf("SweepTurnEvents: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected 1 turn deleted, got %d", deleted)
	}

	// Verify old is gone
	oldR, _ := s.LookupTurn(ctx, turnOld)
	if oldR != nil {
		t.Errorf("expected old turn to be deleted, got %v", oldR)
	}

	// Verify recent and running persist
	recR, _ := s.LookupTurn(ctx, turnRecent)
	if recR == nil {
		t.Errorf("expected recent turn to persist")
	}
	runR, _ := s.LookupTurn(ctx, turnRunning)
	if runR == nil {
		t.Errorf("expected running turn to persist")
	}
}
