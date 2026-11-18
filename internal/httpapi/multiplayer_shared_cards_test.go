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
// private, and so does the model replay.

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

// The model replay for another participant's turn is unchanged: no working
// trace of the owner's turn, cards included, reaches the model.
func TestRedactHistoryForSenderStillOmitsCards(t *testing.T) {
	t.Parallel()

	for _, e := range redactHistoryForSender(sharedCardHistory(), "mate@x.com", "owner@x.com") {
		if e.Type == "tool_call" || e.Type == "tool_result" || e.Type == "reasoning" {
			t.Errorf("replay kept a %s entry (id %d) of another sender's turn", e.Type, e.ID)
		}
	}
}

func TestSanitizeTurnEventSharesLiveCards(t *testing.T) {
	t.Parallel()

	cases := []struct {
		event, data string
		keep        bool
	}{
		{"tool.call", `{"id":"ui1","name":"show_ui","input":"{}","user_email":"owner@x.com"}`, true},
		{"tool.result", `{"id":"ui1","name":"show_ui","text":"UI_DISPLAYED card_id=ui1","card":"{}","user_email":"owner@x.com"}`, true},
		{"tool.call", `{"id":"q1","name":"run_python","input":"{}","user_email":"owner@x.com"}`, false},
		{"tool.result", `{"id":"q1","name":"run_python","text":"rows","user_email":"owner@x.com"}`, false},
		// Only the card events are shared: another event that happens to carry
		// the tool's name is still the sender's.
		{"tool.approval_required", `{"name":"show_ui","user_email":"owner@x.com"}`, false},
		{"reasoning.delta", `{"text":"hm","user_email":"owner@x.com"}`, false},
	}
	for _, tc := range cases {
		got, keep := sanitizeTurnEventForViewer(tc.event, []byte(tc.data), "mate@x.com", "owner@x.com")
		if keep != tc.keep {
			t.Errorf("%s %s: keep = %v, want %v", tc.event, tc.data, keep, tc.keep)
			continue
		}
		if keep && strings.Contains(string(got), "user_email") {
			t.Errorf("%s: delivered frame still carries user_email: %s", tc.event, got)
		}
	}
	// The sender's own frames are untouched by the rule.
	if _, keep := sanitizeTurnEventForViewer("tool.call", []byte(`{"id":"q1","name":"run_python","user_email":"owner@x.com"}`), "owner@x.com", "owner@x.com"); !keep {
		t.Error("the sender lost their own tool.call")
	}
}
