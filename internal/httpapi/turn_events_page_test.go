package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/store"
)

// seedConvWithTurnEvents provisions a user + conversation and writes `turns`
// turns of `perTurn` events each through the production store paths, returning
// the conversation id. Exercised by the /conversations/{id}/events handler test.
func seedConvWithTurnEvents(t *testing.T, st *store.Store, email string, turns, perTurn int) string {
	t.Helper()
	ctx := context.Background()
	seedUser(t, st, email)
	conv, err := st.CreateConversation(ctx, email, "events", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	now := time.Now().Unix()
	for ti := 0; ti < turns; ti++ {
		turnID := fmt.Sprintf("turn_%d", ti)
		if err := st.CreateTurn(ctx, turnID, conv.ID, now+int64(ti)); err != nil {
			t.Fatalf("CreateTurn: %v", err)
		}
		batch := make([]store.TurnEvent, 0, perTurn)
		for ei := 1; ei <= perTurn; ei++ {
			batch = append(batch, store.TurnEvent{
				TurnID:    turnID,
				EventID:   uint64(ei),
				Name:      "content_block_delta",
				Data:      []byte(fmt.Sprintf(`{"turn":%d,"e":%d}`, ti, ei)),
				CreatedAt: now + int64(ti),
			})
		}
		if err := st.InsertTurnEvents(ctx, batch); err != nil {
			t.Fatalf("InsertTurnEvents: %v", err)
		}
	}
	return conv.ID
}

func decodePage(t *testing.T, body []byte) turnEventPageResponse {
	t.Helper()
	var resp turnEventPageResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode page: %v (body=%s)", err, body)
	}
	return resp
}

func TestTurnEventsPage_Ordering_Limit_HasMore(t *testing.T) {
	s := serverFixture(t)
	h := s.Routes()
	const owner = "owner@x.com"
	convID := seedConvWithTurnEvents(t, s.concreteStore(t), owner, 2, 5) // seq 1..10

	// Default direction = desc: newest 4 events (7..10), ascending in the slice.
	w := do(t, h, http.MethodGet, "/conversations/"+convID+"/events?limit=4", nil, owner)
	if w.Code != http.StatusOK {
		t.Fatalf("events: %d body=%s", w.Code, w.Body.String())
	}
	page := decodePage(t, w.Body.Bytes())
	if len(page.Events) != 4 {
		t.Fatalf("expected 4 events, got %d", len(page.Events))
	}
	if page.Events[0].Sequence != 7 || page.Events[3].Sequence != 10 {
		t.Errorf("desc page seqs: got %d..%d, want 7..10", page.Events[0].Sequence, page.Events[3].Sequence)
	}
	// Ascending within the page.
	for i := 1; i < len(page.Events); i++ {
		if page.Events[i].Sequence <= page.Events[i-1].Sequence {
			t.Fatalf("page not ascending: %+v", page.Events)
		}
	}
	if !page.HasMore {
		t.Errorf("expected has_more=true with older events remaining")
	}
	// prev_cursor drives scroll-up; for a full desc page it is the page's low edge.
	if page.PrevCursor != "7" {
		t.Errorf("prev_cursor=%q, want %q", page.PrevCursor, "7")
	}
	// data is round-tripped as raw JSON, not a double-encoded string.
	var payload map[string]int
	if err := json.Unmarshal(page.Events[0].Data, &payload); err != nil {
		t.Fatalf("event data is not raw JSON: %v (%s)", err, page.Events[0].Data)
	}
}

func TestTurnEventsPage_ScrollUpToLastPage(t *testing.T) {
	s := serverFixture(t)
	h := s.Routes()
	const owner = "scroll@x.com"
	convID := seedConvWithTurnEvents(t, s.concreteStore(t), owner, 1, 6) // seq 1..6

	// First desc page of 4 → 3..6, prev_cursor=3.
	w := do(t, h, http.MethodGet, "/conversations/"+convID+"/events?limit=4&direction=desc", nil, owner)
	page := decodePage(t, w.Body.Bytes())
	if page.PrevCursor != "3" || !page.HasMore {
		t.Fatalf("first page: prev_cursor=%q has_more=%v, want 3/true", page.PrevCursor, page.HasMore)
	}

	// Scroll up using prev_cursor → 1..2, last page, has_more=false.
	w = do(t, h, http.MethodGet, "/conversations/"+convID+"/events?limit=4&direction=desc&cursor="+page.PrevCursor, nil, owner)
	page = decodePage(t, w.Body.Bytes())
	if len(page.Events) != 2 || page.Events[0].Sequence != 1 || page.Events[1].Sequence != 2 {
		t.Fatalf("last page seqs wrong: %+v", page.Events)
	}
	if page.HasMore {
		t.Errorf("expected has_more=false on the last page")
	}
	if page.PrevCursor != "" {
		t.Errorf("expected empty prev_cursor at the head, got %q", page.PrevCursor)
	}
}

func TestTurnEventsPage_AscCatchUpRoundTrip(t *testing.T) {
	s := serverFixture(t)
	h := s.Routes()
	const owner = "catchup@x.com"
	convID := seedConvWithTurnEvents(t, s.concreteStore(t), owner, 3, 4) // seq 1..12

	// Asc from cursor 0 walks forward; chase next_cursor to the end.
	var seen []int64
	cursor := ""
	for {
		path := "/conversations/" + convID + "/events?limit=5&direction=asc"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		w := do(t, h, http.MethodGet, path, nil, owner)
		if w.Code != http.StatusOK {
			t.Fatalf("asc page: %d body=%s", w.Code, w.Body.String())
		}
		page := decodePage(t, w.Body.Bytes())
		for _, e := range page.Events {
			seen = append(seen, e.Sequence)
		}
		if !page.HasMore {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != 12 {
		t.Fatalf("asc round-trip saw %d events, want 12: %v", len(seen), seen)
	}
	for i, seq := range seen {
		if seq != int64(i+1) {
			t.Fatalf("asc round-trip out of order at %d: got %d", i, seq)
		}
	}
}

func TestTurnEventsPage_BadRequests(t *testing.T) {
	s := serverFixture(t)
	h := s.Routes()
	const owner = "bad@x.com"
	convID := seedConvWithTurnEvents(t, s.concreteStore(t), owner, 1, 2)

	cases := []struct {
		name  string
		query string
	}{
		{"non-numeric limit", "?limit=abc"},
		{"zero limit", "?limit=0"},
		{"negative limit", "?limit=-5"},
		{"non-numeric cursor", "?cursor=xyz"},
		{"negative cursor", "?cursor=-1"},
		{"bad direction", "?direction=sideways"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, h, http.MethodGet, "/conversations/"+convID+"/events"+tc.query, nil, owner)
			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400, got %d (body=%s)", w.Code, w.Body.String())
			}
		})
	}
}

func TestTurnEventsPage_UnknownConversation(t *testing.T) {
	s := serverFixture(t)
	h := s.Routes()
	seedUser(t, s.concreteStore(t), "ghost@x.com")
	w := do(t, h, http.MethodGet, "/conversations/does-not-exist/events", nil, "ghost@x.com")
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown conversation, got %d (body=%s)", w.Code, w.Body.String())
	}
}

func TestTurnEventsPage_CrossUserIsolation(t *testing.T) {
	s := serverFixture(t)
	h := s.Routes()
	convID := seedConvWithTurnEvents(t, s.concreteStore(t), "alice@x.com", 1, 3)

	// A different authenticated user must not read alice's events — the
	// ownership check (store.Get scoped to user) returns nil → 404.
	seedUser(t, s.concreteStore(t), "mallory@x.com")
	w := do(t, h, http.MethodGet, "/conversations/"+convID+"/events", nil, "mallory@x.com")
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for non-owner, got %d (body=%s)", w.Code, w.Body.String())
	}
}

func TestTurnEventsPage_LastPageHasMoreFalse(t *testing.T) {
	s := serverFixture(t)
	h := s.Routes()
	const owner = "lastpage@x.com"
	convID := seedConvWithTurnEvents(t, s.concreteStore(t), owner, 1, 3) // seq 1..3

	// limit larger than the row count → everything, has_more=false.
	w := do(t, h, http.MethodGet, "/conversations/"+convID+"/events?limit=50&direction=asc", nil, owner)
	page := decodePage(t, w.Body.Bytes())
	if len(page.Events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(page.Events))
	}
	if page.HasMore {
		t.Errorf("expected has_more=false when the whole conversation fits in one page")
	}
	if page.NextCursor != "" {
		t.Errorf("expected empty next_cursor on the last asc page, got %q", page.NextCursor)
	}
}
