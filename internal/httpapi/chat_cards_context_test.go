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
// of them must be able to update any of the cards: the replay is the same
// history for everyone (TestReplayIsSharedInAMultiplayerChat), and a patrol
// turn is also given the list of the board's panels.

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
