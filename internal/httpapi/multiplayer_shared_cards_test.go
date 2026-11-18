package httpapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// A multiplayer participant's dashboard was empty on a chat whose panels the
// owner's turns had built: the cards went out with the rest of those turns'
// working trace. A show_ui card is what the assistant showed the conversation,
// so every participant gets it; the rest of another sender's trace stays
// private.

func sharedCardHistory() []agent.HistoryEntry {
	return []agent.HistoryEntry{
		{ID: 1, Role: "user", Type: "text", Content: []byte(`{"text":"show the board","sender_email":"owner@x.com"}`), InjectedContext: "\n\n---\nowner files"},
		{ID: 2, Role: "assistant", Type: "reasoning", Content: []byte(`{"text":"thinking"}`)},
		{ID: 3, Role: "assistant", Type: "tool_call", Content: []byte(`{"id":"q1","name":"run_python","input":"{}"}`)},
		{ID: 4, Role: "tool", Type: "tool_result", Content: []byte(`{"id":"q1","name":"run_python","text":"owner connector rows","sender_email":"owner@x.com"}`)},
		{ID: 5, Role: "assistant", Type: "tool_call", Content: []byte(`{"id":"ui1","name":"show_ui","input":"{\"title\":\"Inbox\",\"panel\":\"inbox\",\"components\":[]}"}`)},
		{ID: 6, Role: "tool", Type: "tool_result", Content: []byte(`{"id":"ui1","name":"show_ui","text":"UI_DISPLAYED card_id=ui1","sender_email":"owner@x.com"}`)},
		{ID: 7, Role: "assistant", Type: "tool_call", Content: []byte(`{"id":"ui2","name":"show_ui","input":"{\"from_result\":{\"call_id\":\"q1\"}}"}`)},
		{ID: 8, Role: "tool", Type: "tool_result", Content: []byte(`{"id":"ui2","name":"show_ui","text":"UI_DISPLAYED card_id=ui2","card":"{\"title\":\"Recs\",\"panel\":\"recs\",\"components\":[]}"}`)},
		{ID: 9, Role: "assistant", Type: "text", Content: []byte(`{"text":"Here is the board."}`)},
	}
}

func TestHistoryForParticipantSharesCardsFromAnotherSendersTurn(t *testing.T) {
	t.Parallel()

	out := historyForParticipant(sharedCardHistory(), "mate@x.com", "owner@x.com")
	ids := make([]int64, 0, len(out))
	for _, e := range out {
		ids = append(ids, e.ID)
		if e.InjectedContext != "" {
			t.Errorf("entry %d carries the owner's injected context", e.ID)
		}
		if strings.Contains(string(e.Content), "sender_email") && e.Type == "tool_result" {
			t.Errorf("entry %d still names its sender: %s", e.ID, e.Content)
		}
		if strings.Contains(string(e.Content), "owner connector rows") {
			t.Errorf("entry %d leaks another sender's tool output", e.ID)
		}
	}
	// The user message, both cards (call + result) and the reply; no reasoning,
	// no run_python call or result.
	want := []int64{1, 5, 6, 7, 8, 9}
	if len(ids) != len(want) {
		t.Fatalf("entries = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("entries = %v, want %v", ids, want)
		}
	}
	// The by-reference card keeps the spec the browser draws it from.
	var res agent.ToolResultContent
	if err := json.Unmarshal(out[4].Content, &res); err != nil || !strings.Contains(res.Card, `"panel":"recs"`) {
		t.Errorf("by-reference card lost its spec: %s (err %v)", out[4].Content, err)
	}

	// The owner still gets everything.
	if got := historyForParticipant(sharedCardHistory(), "owner@x.com", "owner@x.com"); len(got) != 9 {
		t.Errorf("owner entries = %d, want 9", len(got))
	}
}

// The model replay for another participant's turn keeps the shown cards too
// (TestReplayKeepsOtherSendersCurrentCards), and still nothing else of the
// owner's working trace.
func TestRedactHistoryForSenderKeepsOnlyCards(t *testing.T) {
	t.Parallel()

	for _, e := range redactHistoryForSender(sharedCardHistory(), "mate@x.com", "owner@x.com") {
		if e.Type == "reasoning" || strings.Contains(string(e.Content), "run_python") || strings.Contains(string(e.Content), "owner connector rows") {
			t.Errorf("replay kept a private %s entry (id %d) of another sender's turn: %s", e.Type, e.ID, e.Content)
		}
	}
}

// A card the server refused (UI_INVALID) or never answered was not shown: its
// raw input and the refusal stay with the sender.
func TestHistoryForParticipantKeepsRefusedCardsPrivate(t *testing.T) {
	t.Parallel()

	entries := []agent.HistoryEntry{
		{ID: 1, Role: "user", Type: "text", Content: []byte(`{"text":"go","sender_email":"owner@x.com"}`)},
		{ID: 2, Role: "assistant", Type: "tool_call", Content: []byte(`{"id":"bad","name":"show_ui","input":"{\"secret\":\"rows\"}"}`)},
		{ID: 3, Role: "tool", Type: "tool_result", Content: []byte(`{"id":"bad","name":"show_ui","text":"UI_INVALID: nope","is_err":true}`)},
		// The retry reuses the id and is shown: only this pair is shared.
		{ID: 4, Role: "assistant", Type: "tool_call", Content: []byte(`{"id":"bad","name":"show_ui","input":"{\"title\":\"ok\",\"components\":[]}"}`)},
		{ID: 5, Role: "tool", Type: "tool_result", Content: []byte(`{"id":"bad","name":"show_ui","text":"UI_DISPLAYED card_id=bad"}`)},
		// Cancelled before its result.
		{ID: 6, Role: "assistant", Type: "tool_call", Content: []byte(`{"id":"open","name":"show_ui","input":"{}"}`)},
		// A result with no call of its own.
		{ID: 7, Role: "tool", Type: "tool_result", Content: []byte(`{"id":"stray","name":"show_ui","text":"UI_DISPLAYED card_id=stray"}`)},
	}
	out := historyForParticipant(entries, "mate@x.com", "owner@x.com")
	ids := make([]int64, 0, len(out))
	for _, e := range out {
		ids = append(ids, e.ID)
		if strings.Contains(string(e.Content), "secret") || strings.Contains(string(e.Content), "UI_INVALID") {
			t.Errorf("entry %d leaks a refused card: %s", e.ID, e.Content)
		}
	}
	if want := []int64{1, 4, 5}; len(ids) != len(want) || ids[0] != want[0] || ids[1] != want[1] || ids[2] != want[2] {
		t.Fatalf("entries = %v, want %v", ids, want)
	}
}

// Live, another sender's call frame is never delivered (the card may yet be
// refused); the UI_DISPLAYED result is, carrying the call's input.
func TestSanitizeTurnEventSharesShownCardsLive(t *testing.T) {
	t.Parallel()

	events := []bufferedEvent{
		{ID: 1, Name: "tool.call", Data: []byte(`{"id":"q1","name":"run_python","input":"{}","user_email":"owner@x.com"}`)},
		{ID: 2, Name: "tool.call", Data: []byte(`{"id":"ui1","name":"show_ui","input":"{\"title\":\"v1\"}","user_email":"owner@x.com"}`)},
		{ID: 3, Name: "tool.result", Data: []byte(`{"id":"ui1","name":"show_ui","text":"UI_INVALID: nope","is_err":true,"user_email":"owner@x.com"}`)},
		{ID: 4, Name: "tool.call", Data: []byte(`{"id":"ui1","name":"show_ui","input":"{\"title\":\"v2\"}","user_email":"owner@x.com"}`)},
		{ID: 5, Name: "tool.result", Data: []byte(`{"id":"ui1","name":"show_ui","text":"UI_DISPLAYED card_id=ui1","is_err":false,"user_email":"owner@x.com"}`)},
		{ID: 6, Name: "tool.result", Data: []byte(`{"id":"q1","name":"run_python","text":"rows","is_err":false,"user_email":"owner@x.com"}`)},
		{ID: 7, Name: "reasoning.delta", Data: []byte(`{"text":"hm","user_email":"owner@x.com"}`)},
	}
	deliver := func(viewer string, withLookup bool) map[uint64]string {
		got := map[uint64]string{}
		for _, e := range events {
			var lookup func(string) (string, bool)
			if withLookup {
				lookup = func(id string) (string, bool) { return showUICallInput(events, id, e.ID) }
			}
			if data, keep := sanitizeTurnEventForViewerWithCards(e.Name, e.Data, viewer, "owner@x.com", lookup); keep {
				got[e.ID] = string(data)
			}
		}
		return got
	}

	mate := deliver("mate@x.com", true)
	if len(mate) != 1 {
		t.Fatalf("participant got frames %v, want only the shown card's result", mate)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(mate[5]), &res); err != nil {
		t.Fatal(err)
	}
	// The input of the call this result answers (the retry), not the refused one.
	if res["input"] != `{"title":"v2"}` || res["user_email"] != nil {
		t.Errorf("shown card frame = %s", mate[5])
	}

	// Without the earlier frames (evicted, or a caller with no lookup) the card
	// is not delivered live; the history read still carries it.
	if got := deliver("mate@x.com", false); len(got) != 0 {
		t.Errorf("no lookup: got %v, want nothing", got)
	}
	// The sender gets every frame, and no input copied onto the result.
	owner := deliver("owner@x.com", true)
	if len(owner) != len(events) || strings.Contains(owner[5], `"input"`) {
		t.Errorf("sender frames = %v", owner)
	}
}

// Two calls can share an id before either is answered (a provider reusing ids
// within one step). Results answer them oldest first, in history and live, so
// the card a participant draws is the one that was shown, and a refused
// call's input is never the one that is published.
func TestSharedCardsPairAReusedIDOldestFirst(t *testing.T) {
	t.Parallel()

	entries := []agent.HistoryEntry{
		{ID: 1, Role: "user", Type: "text", Content: []byte(`{"text":"go","sender_email":"owner@x.com"}`)},
		{ID: 2, Role: "assistant", Type: "tool_call", Content: []byte(`{"id":"dup","name":"show_ui","input":"{\"title\":\"first\"}"}`)},
		{ID: 3, Role: "assistant", Type: "tool_call", Content: []byte(`{"id":"dup","name":"show_ui","input":"{\"secret\":\"second\"}"}`)},
		{ID: 4, Role: "tool", Type: "tool_result", Content: []byte(`{"id":"dup","name":"show_ui","text":"UI_DISPLAYED card_id=dup"}`)},
		{ID: 5, Role: "tool", Type: "tool_result", Content: []byte(`{"id":"dup","name":"show_ui","text":"UI_INVALID: nope","is_err":true}`)},
	}
	out := historyForParticipant(entries, "mate@x.com", "owner@x.com")
	if len(out) != 3 || out[1].ID != 2 || out[2].ID != 4 {
		t.Fatalf("history kept %+v, want the user text, the first call and its result", out)
	}

	events := []bufferedEvent{
		{ID: 2, Name: "tool.call", Data: []byte(`{"id":"dup","name":"show_ui","input":"first","user_email":"owner@x.com"}`)},
		{ID: 3, Name: "tool.call", Data: []byte(`{"id":"dup","name":"show_ui","input":"second","user_email":"owner@x.com"}`)},
		{ID: 4, Name: "tool.result", Data: []byte(`{"id":"dup","name":"show_ui","text":"UI_DISPLAYED card_id=dup","user_email":"owner@x.com"}`)},
		{ID: 5, Name: "tool.result", Data: []byte(`{"id":"dup","name":"show_ui","text":"UI_DISPLAYED card_id=dup","user_email":"owner@x.com"}`)},
	}
	for before, want := range map[uint64]string{4: "first", 5: "second"} {
		if got, ok := showUICallInput(events, "dup", before); !ok || got != want {
			t.Errorf("result %d answers input %q (found %v), want %q", before, got, ok, want)
		}
	}
	if _, ok := showUICallInput(events, "dup", 2); ok {
		t.Error("a result before any call found an input")
	}
}

// A call left unanswered when its turn ended is never answered by a later
// turn: a result there that reuses the id answers that turn's own call. A
// mid-turn steer is not a boundary.
func TestSharedCardsDoNotPairAcrossTurns(t *testing.T) {
	t.Parallel()

	entries := []agent.HistoryEntry{
		{ID: 1, Role: "user", Type: "text", Content: []byte(`{"text":"one","sender_email":"owner@x.com"}`)},
		{ID: 2, Role: "assistant", Type: "tool_call", Content: []byte(`{"id":"ui","name":"show_ui","input":"{\"secret\":\"stale\"}"}`)},
		// Turn one was cancelled before the card's result.
		{ID: 3, Role: "user", Type: "text", Content: []byte(`{"text":"two","sender_email":"owner@x.com"}`)},
		{ID: 4, Role: "assistant", Type: "tool_call", Content: []byte(`{"id":"ui","name":"show_ui","input":"{\"title\":\"shown\"}"}`)},
		{ID: 5, Role: "user", Type: "text", Content: []byte(`{"text":"also this","sender_email":"owner@x.com","steered":true}`)},
		{ID: 6, Role: "tool", Type: "tool_result", Content: []byte(`{"id":"ui","name":"show_ui","text":"UI_DISPLAYED card_id=ui"}`)},
		// After a compaction nothing earlier is waiting either.
		{ID: 7, Role: "assistant", Type: "tool_call", Content: []byte(`{"id":"late","name":"show_ui","input":"{}"}`)},
		{ID: 8, Role: "user", Type: "summary", Content: []byte(`{"text":"summary"}`)},
		{ID: 9, Role: "tool", Type: "tool_result", Content: []byte(`{"id":"late","name":"show_ui","text":"UI_DISPLAYED card_id=late"}`)},
	}
	shared := sharedCardEntries(entries)
	if len(shared) != 2 || !shared[3] || !shared[5] {
		t.Fatalf("shared entry indexes = %v, want the second turn's call (3) and its result (5)", shared)
	}
	for _, e := range historyForParticipant(entries, "mate@x.com", "owner@x.com") {
		if strings.Contains(string(e.Content), "stale") {
			t.Errorf("entry %d publishes the cancelled turn's card input", e.ID)
		}
	}
}
