package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// One stranded turn whose recovery fails does not strand the turns after it:
// RecoverStrandedTurns returns its error, leaves it 'running' for the next
// boot, and still recovers the turns that started after it.
func TestRecovery_OneFailingTurnDoesNotStrandTheRest(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().Unix()
	seed := func(turnID string, startedAt int64) {
		t.Helper()
		conv, err := s.CreateConversation(ctx, "u@example.com", "t", "", "m", false)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.CreateTurn(ctx, turnID, conv.ID, startedAt); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CommitUserMessage(ctx, conv.ID, turnID, userEntry(t, "q")); err != nil {
			t.Fatal(err)
		}
		journalIntent(t, s, turnID, 1, "call-"+turnID, "bash", `{}`)
		journalResult(t, s, turnID, 2, "call-"+turnID, "ok")
	}
	seed("stuck-first", now-60) // older: recovered first
	seed("stuck-second", now)

	// The first turn's projection fails for good (a stand-in for any
	// per-turn failure); the second's does not.
	if _, err := s.db.ExecContext(ctx, `
		CREATE FUNCTION chat.refuse_first_turn() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.turn_id = 'stuck-first' THEN RAISE EXCEPTION 'projection refused'; END IF;
			RETURN NEW;
		END $$;
		CREATE TRIGGER refuse_first_turn BEFORE INSERT ON chat.messages
			FOR EACH ROW EXECUTE FUNCTION chat.refuse_first_turn();`); err != nil {
		t.Fatal(err)
	}

	recs, err := s.RecoverStrandedTurns(ctx)
	if err == nil || !strings.Contains(err.Error(), "stuck-first") || !strings.Contains(err.Error(), "projection refused") {
		t.Fatalf("err = %v, want the first turn's failure", err)
	}
	if len(recs) != 1 || recs[0].TurnID != "stuck-second" {
		t.Fatalf("recovered = %+v, want only stuck-second", recs)
	}
	status := func(turnID string) string {
		t.Helper()
		var st string
		if err := s.db.QueryRowContext(ctx, `SELECT status FROM turns WHERE turn_id = $1`, turnID).Scan(&st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	if st := status("stuck-second"); st != string(TurnStatusError) {
		t.Fatalf("stuck-second status = %q, want %q", st, TurnStatusError)
	}
	if st := status("stuck-first"); st != string(TurnStatusRunning) {
		t.Fatalf("stuck-first status = %q, want it left %q for the next boot", st, TurnStatusRunning)
	}

	// The next boot, with the cause gone, recovers the turn that was left.
	if _, err := s.db.ExecContext(ctx, `DROP TRIGGER refuse_first_turn ON chat.messages`); err != nil {
		t.Fatal(err)
	}
	recs, err = s.RecoverStrandedTurns(ctx)
	if err != nil || len(recs) != 1 || recs[0].TurnID != "stuck-first" {
		t.Fatalf("next boot: recovered = %+v, err = %v; want stuck-first", recs, err)
	}
}

// When a turn's recovery fails, the later stranded turns of the same
// conversation wait for the next boot with it, so the turns are still
// recovered in order: the earlier one first, with its journaled tool calls
// projected, then the later one.
func TestRecovery_FailedTurnHoldsBackItsConversation(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().Unix()
	conv, err := s.CreateConversation(ctx, "u@example.com", "t", "", "m", false)
	if err != nil {
		t.Fatal(err)
	}
	for i, turnID := range []string{"held-first", "held-second"} {
		if err := s.CreateTurn(ctx, turnID, conv.ID, now-int64(60*(1-i))); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CommitUserMessage(ctx, conv.ID, turnID, userEntry(t, "q")); err != nil {
			t.Fatal(err)
		}
		journalIntent(t, s, turnID, 1, "call-"+turnID, "bash", `{}`)
		journalResult(t, s, turnID, 2, "call-"+turnID, "ok")
	}
	if _, err := s.db.ExecContext(ctx, `
		CREATE FUNCTION chat.refuse_held_first() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.turn_id = 'held-first' AND NEW.turn_seq > 1 THEN RAISE EXCEPTION 'projection refused'; END IF;
			RETURN NEW;
		END $$;
		CREATE TRIGGER refuse_held_first BEFORE INSERT ON chat.messages
			FOR EACH ROW EXECUTE FUNCTION chat.refuse_held_first();`); err != nil {
		t.Fatal(err)
	}

	recs, err := s.RecoverStrandedTurns(ctx)
	if err == nil || !strings.Contains(err.Error(), "held-second waits for the next boot") {
		t.Fatalf("err = %v, want the later turn held back", err)
	}
	if len(recs) != 0 {
		t.Fatalf("recovered = %+v, want nothing from this conversation", recs)
	}

	if _, err := s.db.ExecContext(ctx, `DROP TRIGGER refuse_held_first ON chat.messages`); err != nil {
		t.Fatal(err)
	}
	recs, err = s.RecoverStrandedTurns(ctx)
	if err != nil || len(recs) != 2 || recs[0].TurnID != "held-first" || recs[1].TurnID != "held-second" {
		t.Fatalf("next boot: recovered = %+v, err = %v; want both turns in order", recs, err)
	}
	if recs[0].Projected == 0 || recs[1].Projected == 0 {
		t.Fatalf("next boot: projected %d and %d rows, want both turns projected", recs[0].Projected, recs[1].Projected)
	}
}
