package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// seedConvWithEvents creates a user + conversation and inserts `turns`, each
// carrying `perTurn` events, via the production CreateTurn/InsertTurnEvents
// paths. It returns the conversation id and the total number of events written.
// Events are inserted one turn at a time, mirroring how a live conversation
// accumulates them, so the per-conversation `sequence` assignment is exercised
// for real (not hand-stamped).
func seedConvWithEvents(t *testing.T, s *Store, email string, turns, perTurn int) (string, int) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, email, "password123"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	conv, err := s.CreateConversation(ctx, email, "t", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	total := 0
	now := time.Now().Unix()
	for ti := 0; ti < turns; ti++ {
		turnID := fmt.Sprintf("turn_%d", ti)
		if err := s.CreateTurn(ctx, turnID, conv.ID, now+int64(ti)); err != nil {
			t.Fatalf("CreateTurn(%s): %v", turnID, err)
		}
		batch := make([]TurnEvent, 0, perTurn)
		for ei := 1; ei <= perTurn; ei++ {
			batch = append(batch, TurnEvent{
				TurnID:    turnID,
				EventID:   uint64(ei),
				Name:      "delta",
				Data:      []byte(fmt.Sprintf(`{"t":%d,"e":%d}`, ti, ei)),
				CreatedAt: now + int64(ti),
			})
		}
		if err := s.InsertTurnEvents(ctx, batch); err != nil {
			t.Fatalf("InsertTurnEvents(%s): %v", turnID, err)
		}
		total += perTurn
	}
	return conv.ID, total
}

// TestInsertTurnEvents_AssignsSequenceAndTurnIndex verifies the insert path
// derives a contiguous, gap-free per-conversation sequence and copies the
// owning turn's conversation_id + turn_index onto each event row.
func TestInsertTurnEvents_AssignsSequenceAndTurnIndex(t *testing.T) {
	s := newTestStore(t)
	convID, total := seedConvWithEvents(t, s, "seq@example.com", 3, 4) // 12 events
	if total != 12 {
		t.Fatalf("expected 12 events seeded, got %d", total)
	}

	// Read everything back ascending in one big page.
	events, next, err := s.GetTurnEventPage(context.Background(), convID, 0, 100, true)
	if err != nil {
		t.Fatalf("GetTurnEventPage: %v", err)
	}
	if next != 0 {
		t.Errorf("expected nextCursor 0 when the page is not full, got %d", next)
	}
	if len(events) != 12 {
		t.Fatalf("expected 12 events, got %d", len(events))
	}
	// Sequences must be 1..12, contiguous and ascending; conversation_id set;
	// turn_index must be the turn ordinal (0,0,0,0,1,1,1,1,2,2,2,2).
	for i, e := range events {
		wantSeq := int64(i + 1)
		if e.Sequence != wantSeq {
			t.Errorf("event[%d]: sequence=%d, want %d", i, e.Sequence, wantSeq)
		}
		if e.ConversationID != convID {
			t.Errorf("event[%d]: conversation_id=%q, want %q", i, e.ConversationID, convID)
		}
		wantTurnIdx := i / 4
		if e.TurnIndex != wantTurnIdx {
			t.Errorf("event[%d]: turn_index=%d, want %d", i, e.TurnIndex, wantTurnIdx)
		}
	}
}

// TestGetTurnEventPage_Table drives the cursor contract across directions and
// boundaries on a fixed 10-event conversation (sequences 1..10).
func TestGetTurnEventPage_Table(t *testing.T) {
	s := newTestStore(t)
	convID, total := seedConvWithEvents(t, s, "page@example.com", 1, 10) // seq 1..10
	if total != 10 {
		t.Fatalf("expected 10 events, got %d", total)
	}

	type want struct {
		firstSeq   int64 // sequence of the first returned event (0 = expect empty)
		lastSeq    int64 // sequence of the last returned event
		count      int
		nextCursor int64
	}
	cases := []struct {
		name   string
		cursor int64
		limit  int
		asc    bool
		want   want
	}{
		{
			name:   "asc first page (cursor 0)",
			cursor: 0, limit: 4, asc: true,
			want: want{firstSeq: 1, lastSeq: 4, count: 4, nextCursor: 4},
		},
		{
			name:   "asc second page",
			cursor: 4, limit: 4, asc: true,
			want: want{firstSeq: 5, lastSeq: 8, count: 4, nextCursor: 8},
		},
		{
			name:   "asc last partial page (no more)",
			cursor: 8, limit: 4, asc: true,
			want: want{firstSeq: 9, lastSeq: 10, count: 2, nextCursor: 0},
		},
		{
			name:   "asc cursor at boundary (last seq) returns empty",
			cursor: 10, limit: 4, asc: true,
			want: want{count: 0, nextCursor: 0},
		},
		{
			name:   "asc cursor past end returns empty",
			cursor: 999, limit: 4, asc: true,
			want: want{count: 0, nextCursor: 0},
		},
		{
			name:   "desc first page (cursor 0 = from the end)",
			cursor: 0, limit: 4, asc: false,
			// Always returned ascending; newest 4 are 7..10.
			want: want{firstSeq: 7, lastSeq: 10, count: 4, nextCursor: 7},
		},
		{
			name:   "desc second page (scroll up)",
			cursor: 7, limit: 4, asc: false,
			want: want{firstSeq: 3, lastSeq: 6, count: 4, nextCursor: 3},
		},
		{
			name:   "desc last partial page (no more)",
			cursor: 3, limit: 4, asc: false,
			want: want{firstSeq: 1, lastSeq: 2, count: 2, nextCursor: 0},
		},
		{
			name:   "desc cursor at lowest seq returns empty",
			cursor: 1, limit: 4, asc: false,
			want: want{count: 0, nextCursor: 0},
		},
		{
			// A page that exactly fills the limit always advertises a cursor,
			// per the documented contract (nextCursor is 0 only when fewer than
			// `limit` rows came back). The client's follow-up fetch returns empty.
			name:   "full single page advertises a cursor",
			cursor: 0, limit: 10, asc: true,
			want: want{firstSeq: 1, lastSeq: 10, count: 10, nextCursor: 10},
		},
	}

	ctx := context.Background()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events, next, err := s.GetTurnEventPage(ctx, convID, tc.cursor, tc.limit, tc.asc)
			if err != nil {
				t.Fatalf("GetTurnEventPage: %v", err)
			}
			if len(events) != tc.want.count {
				t.Fatalf("count=%d, want %d (events=%v)", len(events), tc.want.count, seqs(events))
			}
			if next != tc.want.nextCursor {
				t.Errorf("nextCursor=%d, want %d", next, tc.want.nextCursor)
			}
			// The returned slice must ALWAYS be ascending regardless of direction.
			for i := 1; i < len(events); i++ {
				if events[i].Sequence <= events[i-1].Sequence {
					t.Errorf("page not ascending at %d: %v", i, seqs(events))
				}
			}
			if tc.want.count > 0 {
				if events[0].Sequence != tc.want.firstSeq {
					t.Errorf("firstSeq=%d, want %d", events[0].Sequence, tc.want.firstSeq)
				}
				if events[len(events)-1].Sequence != tc.want.lastSeq {
					t.Errorf("lastSeq=%d, want %d", events[len(events)-1].Sequence, tc.want.lastSeq)
				}
			}
		})
	}
}

// TestGetTurnEventPage_EmptyConversation: a conversation with no events yields
// an empty page and a zero cursor in both directions.
func TestGetTurnEventPage_EmptyConversation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "empty@example.com", "password123"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	conv, err := s.CreateConversation(ctx, "empty@example.com", "t", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	for _, asc := range []bool{true, false} {
		events, next, err := s.GetTurnEventPage(ctx, conv.ID, 0, 50, asc)
		if err != nil {
			t.Fatalf("GetTurnEventPage(asc=%v): %v", asc, err)
		}
		if len(events) != 0 || next != 0 {
			t.Errorf("asc=%v: expected empty page + zero cursor, got %d events, next=%d", asc, len(events), next)
		}
	}
}

// TestGetTurnEventPage_RoundTrip walks the whole conversation ascending in
// limit-sized pages by chasing nextCursor, and confirms it visits every event
// exactly once in order — the cursor round-trip the issue calls for.
func TestGetTurnEventPage_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	convID, total := seedConvWithEvents(t, s, "roundtrip@example.com", 5, 7) // 35 events
	ctx := context.Background()

	const limit = 8
	var seen []int64
	var cursor int64
	for {
		events, next, err := s.GetTurnEventPage(ctx, convID, cursor, limit, true)
		if err != nil {
			t.Fatalf("GetTurnEventPage: %v", err)
		}
		for _, e := range events {
			seen = append(seen, e.Sequence)
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	if len(seen) != total {
		t.Fatalf("round-trip visited %d events, want %d", len(seen), total)
	}
	for i, seq := range seen {
		if seq != int64(i+1) {
			t.Fatalf("round-trip out of order at %d: got %d, want %d", i, seq, i+1)
		}
	}

	// Now scroll-up from the end (desc) and confirm we visit the same set.
	var seenDesc []int64
	cursor = 0
	for {
		events, next, err := s.GetTurnEventPage(ctx, convID, cursor, limit, false)
		if err != nil {
			t.Fatalf("GetTurnEventPage(desc): %v", err)
		}
		// Prepend each page (ascending) to keep global ascending order.
		seenDesc = append(seqsCopy(events), seenDesc...)
		if next == 0 {
			break
		}
		cursor = next
	}
	if len(seenDesc) != total {
		t.Fatalf("desc round-trip visited %d events, want %d", len(seenDesc), total)
	}
	for i, seq := range seenDesc {
		if seq != int64(i+1) {
			t.Fatalf("desc round-trip out of order at %d: got %d, want %d", i, seq, i+1)
		}
	}
}

func seqs(events []TurnEvent) []int64 {
	out := make([]int64, len(events))
	for i, e := range events {
		out[i] = e.Sequence
	}
	return out
}

func seqsCopy(events []TurnEvent) []int64 { return seqs(events) }
