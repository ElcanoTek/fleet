package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// A multiplayer dashboard chat with a patrol: the owner's turn drew two
// panels, a teammate's turn drew a third, and the patrol refreshed one. Each
// of them must be able to update any of the cards: the replay carries the
// cards whoever's turn drew them, and a patrol turn is also given the
// list of the board's panels.

func cardCall(id int64, callID, spec string) agent.HistoryEntry {
	return agent.HistoryEntry{ID: id, Role: "assistant", Type: "tool_call", Content: fmt.Appendf(nil, `{"id":%q,"name":"show_ui","input":%q}`, callID, spec)}
}

func cardShown(id int64, callID string) agent.HistoryEntry {
	return agent.HistoryEntry{ID: id, Role: "tool", Type: "tool_result", Content: fmt.Appendf(nil, `{"id":%q,"name":"show_ui","text":"UI_DISPLAYED card_id=%s"}`, callID, callID)}
}

func userSaid(id int64, sender, text string) agent.HistoryEntry {
	return agent.HistoryEntry{ID: id, Role: "user", Type: "text", Content: fmt.Appendf(nil, `{"text":%q,"sender_email":%q}`, text, sender)}
}

func teamBoardHistory() []agent.HistoryEntry {
	return []agent.HistoryEntry{
		userSaid(1, "owner@x.com", "build the board"),
		cardCall(2, "c-pacing", `{"title":"Pacing","panel":"pacing","components":[]}`),
		cardShown(3, "c-pacing"),
		cardCall(4, "c-notes", `{"title":"Notes [draft]","components":[]}`),
		cardShown(5, "c-notes"),
		userSaid(6, "mate@x.com", "add a margin panel"),
		cardCall(7, "c-margin", `{"title":"Margin","panel":"margin","components":[]}`),
		cardShown(8, "c-margin"),
		// A form is a step in the conversation, not a card to keep.
		cardCall(9, "c-form", `{"title":"Pick a deal","components":[],"actions":[{"id":"go","label":"Go"}]}`),
		cardShown(10, "c-form"),
		// A refused card drew nothing.
		cardCall(11, "c-bad", `{"title":"Refused","panel":"refused"}`),
		{ID: 12, Role: "tool", Type: "tool_result", Content: []byte(`{"id":"c-bad","name":"show_ui","text":"UI_INVALID: no","is_err":true}`)},
		{ID: 13, Role: "user", Type: "text", Content: []byte(`{"text":"[Scheduled patrol #1]","kind":"chat_patrol","sender_email":"owner@x.com"}`)},
		cardCall(14, "c-pacing-2", `{"title":"Pacing (06:30)","panel":"pacing","components":[]}`),
		cardShown(15, "c-pacing-2"),
	}
}

func TestCurrentConversationCards(t *testing.T) {
	t.Parallel()

	got := currentBoardCards(conversationShownCards(teamBoardHistory()))
	want := []dashboardBoardPanel{
		{Title: "Pacing (06:30)", CardID: "c-pacing-2", Panel: "pacing"},
		{Title: "Notes [draft]", CardID: "c-notes"},
		{Title: "Margin", CardID: "c-margin", Panel: "margin"},
	}
	if len(got) != len(want) {
		t.Fatalf("cards = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("card %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if len(currentBoardCards(conversationShownCards(nil))) != 0 {
		t.Error("an empty history lists cards")
	}
}

func TestCurrentConversationCardsReplacesAndReference(t *testing.T) {
	t.Parallel()

	history := []agent.HistoryEntry{
		userSaid(1, "owner@x.com", "go"),
		cardCall(2, "a", `{"title":"A","components":[]}`),
		cardShown(3, "a"),
		cardCall(4, "b", `{"title":"B","panel":"b","components":[]}`),
		cardShown(5, "b"),
		// A refresh by replaces keeps the slot; adding a key moves it there.
		cardCall(6, "a2", `{"title":"A v2","replaces":"a","components":[]}`),
		cardShown(7, "a2"),
		cardCall(8, "a3", `{"title":"A v3","replaces":"a2","panel":"alpha","components":[]}`),
		cardShown(9, "a3"),
		// Drawn by reference: the spec is on the result, the key beside the
		// reference on the call.
		cardCall(10, "r", `{"from_result":{"call_id":"q"},"panel":"recs"}`),
		{ID: 11, Role: "tool", Type: "tool_result", Content: []byte(`{"id":"r","name":"show_ui","text":"UI_DISPLAYED card_id=r","card":"{\"title\":\"Recs\",\"components\":[]}"}`)},
		// A form that replaces a panel keeps the panel's slot and key, as on
		// the web's board.
		cardCall(12, "f", `{"title":"Edit B","replaces":"b","components":[],"actions":[{"id":"save","label":"Save"}]}`),
		cardShown(13, "f"),
		// Refining a card into a key another panel holds merges the two.
		cardCall(14, "m", `{"title":"Recs merged","replaces":"a3","panel":"recs","components":[]}`),
		cardShown(15, "m"),
	}
	got := currentBoardCards(conversationShownCards(history))
	want := []dashboardBoardPanel{
		{Title: "Edit B", CardID: "f", Panel: "b"},
		{Title: "Recs merged", CardID: "m", Panel: "recs"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("cards = %+v, want %+v", got, want)
	}
}

func TestCurrentConversationCardsIsBounded(t *testing.T) {
	t.Parallel()

	const total = maxDashboardBoardPanels + 6
	history := make([]agent.HistoryEntry, 0, 1+2*total+2)
	history = append(history, userSaid(1, "owner@x.com", "go"))
	for i := range total {
		id := fmt.Sprintf("c%02d", i)
		history = append(history, cardCall(int64(2*i+2), id, fmt.Sprintf(`{"title":"T%d","panel":"p%d","components":[]}`, i, i)), cardShown(int64(2*i+3), id))
	}
	// The oldest panel is refreshed last, so it stays; the next ones go.
	history = append(history, cardCall(900, "fresh", `{"title":"T0 new","panel":"p0","components":[]}`), cardShown(901, "fresh"))
	got := currentBoardCards(conversationShownCards(history))
	if len(got) != maxDashboardBoardPanels {
		t.Fatalf("cards = %d, want %d", len(got), maxDashboardBoardPanels)
	}
	if got[0].CardID != "fresh" || got[1].Panel != "p7" || got[len(got)-1].Panel != fmt.Sprintf("p%d", total-1) {
		t.Errorf("kept %+v … %+v, want the most recently updated in first-shown order", got[:2], got[len(got)-1])
	}
}

func TestAppendConversationCardsBlock(t *testing.T) {
	t.Parallel()

	history := teamBoardHistory()
	got := appendConversationCardsBlock("base", agent.InputKindChatPatrol, history)
	for _, want := range []string{
		"base\n\n[Cards in this chat:",
		`1. "Pacing (06:30)" (card_id c-pacing-2): panel "pacing" — update with panel "pacing"`,
		`2. "Notes draft" (card_id c-notes): no panel key — update with replaces="c-notes"`,
		`3. "Margin" (card_id c-margin): panel "margin" — update with panel "margin"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("patrol note lacks %q:\n%s", want, got)
		}
	}
	if !strings.HasSuffix(got, "]") || strings.Contains(got, "Refused") || strings.Contains(got, "Pick a deal") {
		t.Errorf("patrol note is malformed or lists a card that is not on the board:\n%s", got)
	}

	// Unchanged: a person's message (its replay holds the cards), an
	// approval-resume turn, and a patrol in a chat with no cards.
	for name, got := range map[string]string{
		"user turn": appendConversationCardsBlock("base", "", history),
		"resume":    appendConversationCardsBlock("base", agent.InputKindApprovalResume, history),
		"no cards":  appendConversationCardsBlock("base", agent.InputKindChatPatrol, history[:1]),
	} {
		if got != "base" {
			t.Errorf("%s: injected = %q, want it unchanged", name, got)
		}
	}
}

// Anyone in a team chat can ask for a card to be changed, and the assistant
// must work from the card that is there. So the replay for one sender's turn
// carries the cards of every other sender's turn, contents included,
// and nothing else of their working trace.
func TestReplayKeepsOtherSendersCards(t *testing.T) {
	t.Parallel()

	history := []agent.HistoryEntry{
		userSaid(1, "owner@x.com", "build the board"),
		{ID: 2, Role: "assistant", Type: "reasoning", Content: []byte(`{"text":"owner thinking"}`)},
		{ID: 3, Role: "assistant", Type: "tool_call", Content: []byte(`{"id":"q","name":"run_python","input":"{}"}`)},
		{ID: 4, Role: "tool", Type: "tool_result", Content: []byte(`{"id":"q","name":"run_python","text":"owner connector rows","sender_email":"owner@x.com"}`)},
		cardCall(5, "old", `{"title":"Pacing v1","panel":"pacing","components":[{"type":"text","text":"yesterday"}]}`),
		cardShown(6, "old"),
		cardCall(7, "ref", `{"from_result":{"call_id":"q"},"panel":"recs"}`),
		{ID: 8, Role: "tool", Type: "tool_result", Content: []byte(`{"id":"ref","name":"show_ui","text":"UI_DISPLAYED card_id=ref","sender_email":"owner@x.com","card":"{\"title\":\"Recs\",\"panel\":\"recs\",\"components\":[{\"type\":\"text\",\"text\":\"three recs\"}]}"}`)},
		cardCall(9, "bad", `{"title":"Refused","secret":"rows"}`),
		{ID: 10, Role: "tool", Type: "tool_result", Content: []byte(`{"id":"bad","name":"show_ui","text":"UI_INVALID: no","is_err":true}`)},
		cardCall(11, "new", `{"title":"Pacing v2","panel":"pacing","components":[{"type":"text","text":"today"}]}`),
		cardShown(12, "new"),
		{ID: 13, Role: "assistant", Type: "text", Content: []byte(`{"text":"Board is up."}`)},
		userSaid(14, "mate@x.com", "add yesterday to the pacing chart"),
	}

	out := redactHistoryForSender(history, "mate@x.com", "owner@x.com")
	ids := make([]int64, 0, len(out))
	for _, e := range out {
		ids = append(ids, e.ID)
		for _, private := range []string{"owner thinking", "owner connector rows", "Refused", "sender_email\":\"owner", `\"card\"`} {
			if e.Role != "user" && strings.Contains(string(e.Content), private) {
				t.Errorf("entry %d carries %q: %s", e.ID, private, e.Content)
			}
		}
	}
	// The owner's message, every card that was shown (the replaced pacing
	// card too, as in the owner's own replay), the reply and the teammate's
	// message: not the reasoning, the script or the refused card.
	if want := []int64{1, 5, 6, 7, 8, 11, 12, 13, 14}; !slices.Equal(ids, want) {
		t.Fatalf("replayed entries = %v, want %v", ids, want)
	}
	// The current card is replayed with its contents, and the by-reference
	// card with the spec it was drawn from.
	var call agent.ToolCallContent
	if err := json.Unmarshal(out[5].Content, &call); err != nil || !strings.Contains(call.Input, `"text":"today"`) {
		t.Errorf("the current card lost its contents: %s (err %v)", out[3].Content, err)
	}
	if err := json.Unmarshal(out[3].Content, &call); err != nil || !strings.Contains(call.Input, "three recs") || strings.Contains(call.Input, "from_result") {
		t.Errorf("the by-reference card is not replayed with its spec: %s (err %v)", out[1].Content, err)
	}

	// The owner's own replay is untouched: every entry, as stored.
	own := redactHistoryForSender(history, "owner@x.com", "owner@x.com")
	if len(own) != len(history) || string(own[6].Content) != string(history[6].Content) {
		t.Errorf("the owner's replay changed: %d entries", len(own))
	}
}

// The replay for one sender only ever grows at its end: a later card that
// replaces an earlier one removes nothing from the earlier part. The
// compaction records a turn persists are positions in that turn's replay, so
// a card dropping out later would make them cut in the wrong place.
func TestReplayOfOtherSendersCardsIsAppendOnly(t *testing.T) {
	t.Parallel()

	history := make([]agent.HistoryEntry, 0, 12)
	history = append(history,
		userSaid(1, "owner@x.com", "build the board"),
		cardCall(2, "v1", `{"title":"Pacing v1","panel":"pacing","components":[]}`),
		cardShown(3, "v1"),
		cardCall(4, "note", `{"title":"Note","components":[]}`),
		cardShown(5, "note"),
		userSaid(6, "mate@x.com", "what changed?"),
		agent.HistoryEntry{ID: 7, Role: "assistant", Type: "text", Content: []byte(`{"text":"Nothing yet."}`)},
	)
	before := redactHistoryForSender(history, "mate@x.com", "owner@x.com")
	history = append(history,
		userSaid(8, "owner@x.com", "refresh"),
		cardCall(9, "v2", `{"title":"Pacing v2","panel":"pacing","components":[]}`),
		cardShown(10, "v2"),
		cardCall(11, "note2", `{"title":"Note 2","replaces":"note","components":[]}`),
		cardShown(12, "note2"),
	)
	after := redactHistoryForSender(history, "mate@x.com", "owner@x.com")
	if len(after) != len(before)+5 {
		t.Fatalf("replay grew from %d to %d entries, want %d", len(before), len(after), len(before)+5)
	}
	for i := range before {
		if after[i].ID != before[i].ID || string(after[i].Content) != string(before[i].Content) {
			t.Fatalf("entry %d of the earlier replay changed: %d -> %d", i, before[i].ID, after[i].ID)
		}
	}
}

// patrolCardEngine stands in for the model on a patrol turn: it records what
// the turn was told and refreshes a panel with show_ui, as a patrol that
// keeps a dashboard current does.
type patrolCardEngine struct {
	fakeEngine
	injected string
	result   string
}

func (e *patrolCardEngine) RunTurn(ctx context.Context, in TurnInput, sink agent.EventSink) (*TurnResult, error) {
	resp, err := tools.NewShowUITool().Run(ctx, fantasy.ToolCall{
		ID:    "c-pacing-3",
		Name:  tools.ShowUIToolName,
		Input: `{"title":"Pacing (now)","replaces":"c-pacing-2","panel":"pacing","components":[{"type":"text","text":"on pace"}]}`,
	})
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.injected = in.InjectedContext
	e.result = resp.Content
	e.mu.Unlock()
	return e.fakeEngine.RunTurn(ctx, in, sink)
}

// cancelRecordingStore records the card ids whose countdowns a turn stopped.
type cancelRecordingStore struct {
	*scheduleFakeStore
	mu        sync.Mutex
	cancelled []string
}

func (s *cancelRecordingStore) CancelActiveUIAutoSubmits(ctx context.Context, convID, callID string) ([]store.UIAutoSubmit, int, error) {
	s.mu.Lock()
	s.cancelled = append(s.cancelled, callID)
	s.mu.Unlock()
	return s.scheduleFakeStore.CancelActiveUIAutoSubmits(ctx, convID, callID)
}

// TestPatrolTurnUpdatesDashboardCards is the whole path a scheduled run takes
// to keep a board current: the fired patrol turn is told the chat's cards,
// show_ui draws the refreshed panel, and the card it replaces has its
// countdown stopped like any other turn's would.
func TestPatrolTurnUpdatesDashboardCards(t *testing.T) {
	t.Parallel()

	st := &cancelRecordingStore{scheduleFakeStore: newScheduleFakeStore()}
	eng := &patrolCardEngine{}
	srv := newDefaultChatServer(t, eng, st)
	srv.chatSchedules = &fakeChatScheduleEngine{st: st.scheduleFakeStore, srv: srv}
	defer srv.StopBackground()
	h := srv.Routes()

	ctx := context.Background()
	const owner = "trader@elcanotek.com"
	conv, err := st.CreateConversation(ctx, owner, "Opti workbench", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if _, err := st.AppendHistory(ctx, conv.ID, teamBoardHistory()); err != nil {
		t.Fatalf("AppendHistory: %v", err)
	}

	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req = req.WithContext(asUser(req.Context(), owner))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	rec := do(http.MethodPost, "/conversations/"+conv.ID+"/schedules", `{"title":"Morning board","prompt":"Refresh the board.","cron":"30 6 * * *","timezone":"UTC"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /schedules = %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Schedule store.ChatSchedule `json:"schedule"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode schedule: %v", err)
	}
	if rec := do(http.MethodPost, "/conversations/"+conv.ID+"/schedules/"+created.Schedule.ID+"/run-now", `{}`); rec.Code != http.StatusOK {
		t.Fatalf("run-now = %d: %s", rec.Code, rec.Body.String())
	}
	srv.DrainTurns(ctx)

	eng.mu.Lock()
	injected, result := eng.injected, eng.result
	eng.mu.Unlock()
	if !strings.Contains(injected, "[Cards in this chat:") || !strings.Contains(injected, `panel "pacing" — update with panel "pacing"`) || !strings.Contains(injected, `panel "margin"`) {
		t.Errorf("the patrol turn was not told the chat's cards:\n%s", injected)
	}
	if !strings.HasPrefix(result, "UI_DISPLAYED card_id=c-pacing-3") {
		t.Errorf("show_ui on a patrol turn = %q, want the card displayed", result)
	}
	st.mu.Lock()
	cancelled := slices.Clone(st.cancelled)
	st.mu.Unlock()
	if !slices.Contains(cancelled, "c-pacing-2") {
		t.Errorf("the replaced card's countdown was not stopped on a patrol turn; cancelled = %v", cancelled)
	}
	if active, _ := st.ListActiveUIAutoSubmits(ctx, conv.ID); len(active) != 0 {
		t.Errorf("a patrol turn armed an auto-submit countdown: %+v", active)
	}
}

// A refresh that moves a panel to a new key retires the old key: a later
// card that uses the old key opens a new panel, listed after the others, as
// the web's board places it.
func TestCurrentConversationCardsRetiresAMovedKey(t *testing.T) {
	t.Parallel()

	history := []agent.HistoryEntry{
		userSaid(1, "owner@x.com", "go"),
		cardCall(2, "a", `{"title":"A","panel":"a","components":[]}`),
		cardShown(3, "a"),
		cardCall(4, "x", `{"title":"X","panel":"x","components":[]}`),
		cardShown(5, "x"),
		cardCall(6, "b", `{"title":"B","panel":"b","replaces":"a","components":[]}`),
		cardShown(7, "b"),
		cardCall(8, "c", `{"title":"C","panel":"a","components":[]}`),
		cardShown(9, "c"),
	}
	got := currentBoardCards(conversationShownCards(history))
	want := []dashboardBoardPanel{
		{Title: "B", CardID: "b", Panel: "b"},
		{Title: "X", CardID: "x", Panel: "x"},
		{Title: "C", CardID: "c", Panel: "a"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("cards = %+v, want %+v", got, want)
	}
}

// The replay pairs calls with results by id alone, so a teammate's card whose
// id one of the sender's own tool calls also has is left out of the replay
// rather than handed the wrong result.
func TestReplayDropsACardWhoseIDCollidesWithTheSendersCall(t *testing.T) {
	t.Parallel()

	history := []agent.HistoryEntry{
		userSaid(1, "owner@x.com", "build the board"),
		cardCall(2, "call_1", `{"title":"Pacing","panel":"pacing","components":[]}`),
		cardShown(3, "call_1"),
		cardCall(4, "call_2", `{"title":"Margin","panel":"margin","components":[]}`),
		cardShown(5, "call_2"),
		userSaid(6, "mate@x.com", "run the numbers"),
		{ID: 7, Role: "assistant", Type: "tool_call", Content: []byte(`{"id":"call_1","name":"run_python","input":"{}"}`)},
		{ID: 8, Role: "tool", Type: "tool_result", Content: []byte(`{"id":"call_1","name":"run_python","text":"42","sender_email":"mate@x.com"}`)},
	}
	ids := make([]int64, 0, len(history))
	for _, e := range redactHistoryForSender(history, "mate@x.com", "owner@x.com") {
		ids = append(ids, e.ID)
	}
	if want := []int64{1, 4, 5, 6, 7, 8}; !slices.Equal(ids, want) {
		t.Fatalf("replayed entries = %v, want %v (the colliding card dropped, the other kept)", ids, want)
	}
}

// A call from before the latest summary is not sent to the model, so it
// cannot collide with a card drawn after it.
func TestReplayKeepsACardWhoseIDOnlyCollidesBeforeTheSummary(t *testing.T) {
	t.Parallel()

	history := []agent.HistoryEntry{
		userSaid(1, "owner@x.com", "run the numbers"),
		{ID: 2, Role: "assistant", Type: "tool_call", Content: []byte(`{"id":"call_1","name":"run_python","input":"{}"}`)},
		{ID: 3, Role: "tool", Type: "tool_result", Content: []byte(`{"id":"call_1","name":"run_python","text":"42"}`)},
		{ID: 4, Role: "assistant", Type: "summary", Content: []byte(`{"text":"so far"}`)},
		userSaid(5, "mate@x.com", "add a pacing panel"),
		cardCall(6, "call_1", `{"title":"Pacing","panel":"pacing","components":[]}`),
		cardShown(7, "call_1"),
	}
	kept := false
	for _, e := range redactHistoryForSender(history, "owner@x.com", "owner@x.com") {
		kept = kept || e.ID == 6
	}
	if !kept {
		t.Error("a call from before the summary made the replay drop a teammate's later card")
	}
}
