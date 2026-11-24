package httpapi

// The windowed read of GET /conversations/{id} (docs/CHAT-HISTORY-WINDOW.md).
//
// What is pinned here: a caller that names no window gets exactly the response
// it always got; a window is a suffix of that same response, cut where a turn
// opens, for every participant; the cursor pages back through the whole
// history; and what a window leaves out still reaches the views built from the
// whole conversation (dashboard panels, card answers, cost totals).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// conversationRead is the part of the response these tests read.
type conversationRead struct {
	Conversation json.RawMessage        `json:"conversation"`
	History      []historyEntryResponse `json:"history"`
	Page         *historyPage           `json:"history_page"`
}

const windowTestOwner = "owner@x.com"

func getConversation(t *testing.T, h http.Handler, id, query string) (conversationRead, map[string]json.RawMessage) {
	t.Helper()
	w := do(t, h, http.MethodGet, "/conversations/"+id+query, nil, windowTestOwner)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s as %s: %d %s", query, windowTestOwner, w.Code, w.Body.String())
	}
	var read conversationRead
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &read); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &keys); err != nil {
		t.Fatalf("decode keys: %v", err)
	}
	return read, keys
}

func historyJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func hEntry(t *testing.T, role, typ string, v any) agent.HistoryEntry {
	t.Helper()
	return agent.HistoryEntry{Role: role, Type: typ, Content: historyJSON(t, v)}
}

// windowTurn is one turn: its prompt, a tool call with a private result, a
// reply and the turn summary.
func windowTurn(t *testing.T, n int, sender string) []agent.HistoryEntry {
	t.Helper()
	call := fmt.Sprintf("call-%d", n)
	return []agent.HistoryEntry{
		{Role: "user", Type: "text", Content: historyJSON(t, agent.TextContent{Text: fmt.Sprintf("prompt %d", n), SenderEmail: sender}), InjectedContext: fmt.Sprintf("\n\n---\nfiles of turn %d", n)},
		hEntry(t, "assistant", "tool_call", agent.ToolCallContent{ID: call, Name: "run_python", Input: `{"code":"print(1)"}`}),
		hEntry(t, "tool", "tool_result", agent.ToolResultContent{ID: call, Name: "run_python", Text: fmt.Sprintf("private rows of turn %d", n)}),
		hEntry(t, "assistant", "text", agent.TextContent{Text: fmt.Sprintf("reply %d", n)}),
		hEntry(t, "assistant", "turn_summary", agent.TurnSummaryContent{CostUSD: 0.25, PromptTokens: 100, CachedTokens: 400, DurationMs: 10}),
	}
}

func appendEntries(t *testing.T, st chatStore, convID string, entries []agent.HistoryEntry) []int64 {
	t.Helper()
	ids, err := st.AppendHistory(context.Background(), convID, entries)
	if err != nil {
		t.Fatalf("append history: %v", err)
	}
	return ids
}

func responseIDs(entries []historyEntryResponse) []int64 {
	ids := make([]int64, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	return ids
}

// A caller that names no window gets the response it always got: the whole
// history and no history_page key. omit_history is unchanged too.
func TestConversationGetWithoutAWindowIsUnchanged(t *testing.T) {
	t.Parallel()
	s := serverFixture(t)
	h := s.Routes()
	id := newRouteProbeConversationAs(t, h)
	all := make([]int64, 0, 40)
	for n := range 8 {
		all = append(all, appendEntries(t, s.store, id, windowTurn(t, n, ""))...)
	}

	read, keys := getConversation(t, h, id, "")
	if !reflect.DeepEqual(responseIDs(read.History), all) {
		t.Fatalf("history ids %v, want every stored entry %v", responseIDs(read.History), all)
	}
	if _, ok := keys["history_page"]; ok {
		t.Error("a read with no window carries history_page")
	}
	wantKeys := []string{
		"conversation", "branch_origin", "history", "pending_approvals", "resolved_approvals",
		"pending_memory_proposals", "schedules", "admin_note", "ui_autosubmits",
		"autonomous_accepted", "autonomous_policy_version",
	}
	if len(keys) != len(wantKeys) {
		t.Errorf("response has %d keys, want %d: %v", len(keys), len(wantKeys), keys)
	}
	for _, k := range wantKeys {
		if _, ok := keys[k]; !ok {
			t.Errorf("response lost key %q", k)
		}
	}
	// The owner's own entries keep their injected context and tool output.
	if read.History[0].InjectedContext == "" || !strings.Contains(string(read.History[2].Content), "private rows of turn 0") {
		t.Error("the whole-history read lost a field of the owner's own entries")
	}

	_, keys = getConversation(t, h, id, "?omit_history=1")
	if string(keys["history"]) != "[]" {
		t.Errorf("omit_history history = %s, want []", keys["history"])
	}
	if _, ok := keys["history_page"]; ok {
		t.Error("omit_history carries history_page")
	}
	// An empty window parameter is no window.
	read, keys = getConversation(t, h, id, "?limit=&before=&from=")
	if _, ok := keys["history_page"]; ok || len(read.History) != len(all) {
		t.Error("empty window parameters changed the read")
	}
}

func newRouteProbeConversationAs(t *testing.T, h http.Handler) string {
	t.Helper()
	w := do(t, h, http.MethodPost, "/conversations", map[string]string{"title": "window", "persona": "generic"}, windowTestOwner)
	if w.Code != http.StatusOK {
		t.Fatalf("create conversation: %d body=%s", w.Code, w.Body.String())
	}
	var conv store.Conversation
	if err := json.Unmarshal(w.Body.Bytes(), &conv); err != nil {
		t.Fatalf("decode conversation: %v", err)
	}
	return conv.ID
}

// The first window is the newest turns; following next_before pages back to
// the start, and the pages together are the whole-history read, entry for
// entry and byte for byte.
func TestConversationGetWindowPagesBackToTheWholeHistory(t *testing.T) {
	t.Parallel()
	s := serverFixture(t)
	h := s.Routes()
	id := newRouteProbeConversationAs(t, h)
	for n := range 9 {
		appendEntries(t, s.store, id, windowTurn(t, n, ""))
	}
	full, _ := getConversation(t, h, id, "")

	first, _ := getConversation(t, h, id, "?limit=7")
	if first.Page == nil || !first.Page.HasMore {
		t.Fatalf("first window page = %+v, want has_more", first.Page)
	}
	// Seven entries reach into the second-newest turn: both turns, whole.
	if len(first.History) != 10 {
		t.Fatalf("first window holds %d entries, want two whole turns (10)", len(first.History))
	}
	if first.History[0].Role != "user" || first.Page.NextBefore != first.History[0].ID {
		t.Errorf("window starts at a %s entry %d, next_before %d: want a turn's opening message", first.History[0].Role, first.History[0].ID, first.Page.NextBefore)
	}
	if !reflect.DeepEqual(first.History, full.History[len(full.History)-10:]) {
		t.Error("the first window is not the tail of the whole-history read")
	}

	pages := [][]historyEntryResponse{first.History}
	page := first.Page
	for range 20 {
		if !page.HasMore {
			break
		}
		read, _ := getConversation(t, h, id, fmt.Sprintf("?limit=7&before=%d", page.NextBefore))
		if read.Page == nil || len(read.History) == 0 {
			t.Fatalf("page before %d: %+v with %d entries", page.NextBefore, read.Page, len(read.History))
		}
		if last := read.History[len(read.History)-1].ID; last >= page.NextBefore {
			t.Fatalf("page before %d holds entry %d", page.NextBefore, last)
		}
		pages = append([][]historyEntryResponse{read.History}, pages...)
		page = read.Page
	}
	if page.HasMore {
		t.Fatal("paging never reached the start")
	}
	if page.Earlier != nil {
		t.Error("the oldest page describes entries earlier than the first")
	}
	var joined []historyEntryResponse
	for _, p := range pages {
		joined = append(joined, p...)
	}
	if !reflect.DeepEqual(joined, full.History) {
		t.Errorf("the pages (%d entries) are not the whole history (%d)", len(joined), len(full.History))
	}

	// A limit past the cap is clamped, not refused; one that covers the
	// history returns it all with nothing more.
	all, _ := getConversation(t, h, id, "?limit=100000")
	if all.Page == nil || all.Page.HasMore || !reflect.DeepEqual(all.History, full.History) {
		t.Errorf("limit past the cap: page %+v, %d entries", all.Page, len(all.History))
	}
	// from re-reads a held window, and a turn appended since is part of it.
	appendEntries(t, s.store, id, windowTurn(t, 99, ""))
	again, _ := getConversation(t, h, id, fmt.Sprintf("?from=%d", first.Page.NextBefore))
	if again.Page == nil || again.Page.NextBefore != first.Page.NextBefore || len(again.History) != len(first.History)+5 {
		t.Errorf("from re-read: page %+v with %d entries, want the same start and %d", again.Page, len(again.History), len(first.History)+5)
	}
	// from an id inside a turn starts at that turn's opening message.
	mid, _ := getConversation(t, h, id, fmt.Sprintf("?from=%d", first.History[2].ID))
	if mid.Page.NextBefore != first.Page.NextBefore {
		t.Errorf("from a mid-turn id started at %d, want the turn's opening message %d", mid.Page.NextBefore, first.Page.NextBefore)
	}
}

func TestConversationGetWindowRejectsBadParameters(t *testing.T) {
	t.Parallel()
	s := serverFixture(t)
	h := s.Routes()
	id := newRouteProbeConversationAs(t, h)
	appendEntries(t, s.store, id, windowTurn(t, 0, ""))
	for _, q := range []string{
		"?limit=0", "?limit=-5", "?limit=abc", "?limit=1.5",
		"?before=10", "?limit=5&before=0", "?limit=5&before=x",
		"?from=0", "?from=-1", "?from=x", "?from=5&limit=5", "?from=5&before=9",
	} {
		w := do(t, h, http.MethodGet, "/conversations/"+id+q, nil, windowTestOwner)
		if w.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", q, w.Code)
		}
	}
}

func TestParseHistoryWindow(t *testing.T) {
	t.Parallel()
	req, windowed, err := parseHistoryWindow(url.Values{})
	if err != nil || windowed || req != (historyWindowRequest{}) {
		t.Errorf("no parameters = %+v, %v, %v", req, windowed, err)
	}
	req, windowed, err = parseHistoryWindow(url.Values{"limit": {"99999"}, "before": {"42"}})
	if err != nil || !windowed || req.limit != maxHistoryWindowLimit || req.before != 42 {
		t.Errorf("limit past the cap = %+v, %v, %v", req, windowed, err)
	}
	req, windowed, err = parseHistoryWindow(url.Values{"from": {"7"}})
	if err != nil || !windowed || req.from != 7 || req.limit != 0 {
		t.Errorf("from = %+v, %v, %v", req, windowed, err)
	}
}

// The window is behind the same gate as the whole read: someone who cannot
// open the conversation gets the same 404 whatever window they name.
func TestConversationGetWindowKeepsTheAccessGate(t *testing.T) {
	t.Parallel()
	s := serverFixture(t)
	h := s.Routes()
	id := newRouteProbeConversationAs(t, h)
	for n := range 4 {
		appendEntries(t, s.store, id, windowTurn(t, n, ""))
	}
	whole := do(t, h, http.MethodGet, "/conversations/"+id, nil, "stranger@x.com")
	if whole.Code != http.StatusNotFound {
		t.Fatalf("a stranger's whole read = %d, want 404", whole.Code)
	}
	for _, q := range []string{"?limit=5", "?limit=5&before=999999", "?from=1", "?limit=abc"} {
		w := do(t, h, http.MethodGet, "/conversations/"+id+q, nil, "stranger@x.com")
		if w.Code != whole.Code || w.Body.String() != whole.Body.String() {
			t.Errorf("a stranger's GET %s = %d %q, want the whole read's %d %q", q, w.Code, w.Body.String(), whole.Code, whole.Body.String())
		}
		if strings.Contains(w.Body.String(), "private rows") || strings.Contains(w.Body.String(), "history_page") {
			t.Errorf("a stranger's GET %s leaked history", q)
		}
	}
}

// multiSenderHistory is a multiplayer conversation: the owner and a teammate
// take turns, each with a private working trace and a card on the board; one
// approval is settled turns after its call by a row that names no sender.
func multiSenderHistory(t *testing.T) []agent.HistoryEntry {
	t.Helper()
	const owner, mate = "owner@x.com", "mate@x.com"
	var out []agent.HistoryEntry
	card := func(id, input string) []agent.HistoryEntry {
		return []agent.HistoryEntry{
			hEntry(t, "assistant", "tool_call", agent.ToolCallContent{ID: id, Name: tools.ShowUIToolName, Input: input}),
			hEntry(t, "tool", "tool_result", agent.ToolResultContent{ID: id, Name: tools.ShowUIToolName, Text: "UI_DISPLAYED card_id=" + id}),
		}
	}
	for n := range 12 {
		sender := owner
		if n%2 == 1 {
			sender = mate
		}
		turn := windowTurn(t, n, sender)
		reply, summary := turn[3], turn[4]
		out = append(out, turn[:3]...)
		switch n {
		case 1:
			// The teammate's turn stages an approval-gated call…
			out = append(out, hEntry(t, "assistant", "tool_call", agent.ToolCallContent{ID: "send-1", Name: "send_email", Input: `{"to":"a@b.c"}`}))
		case 2:
			out = append(out, card("ui-pace", `{"title":"Pacing","panel":"pacing","components":[{"type":"text","text":"v1"}]}`)...)
		case 3:
			out = append(out, card("ui-form", `{"title":"Budget","components":[{"type":"text_input","id":"amount","label":"Amount"}]}`)...)
			// …a refused card draws nothing and stays with its sender.
			out = append(out,
				hEntry(t, "assistant", "tool_call", agent.ToolCallContent{ID: "ui-bad", Name: tools.ShowUIToolName, Input: `{"title":"Broken"}`}),
				hEntry(t, "tool", "tool_result", agent.ToolResultContent{ID: "ui-bad", Name: tools.ShowUIToolName, Text: "UI_INVALID: no components", IsErr: true}),
			)
		case 5:
			out = append(out, card("ui-pace-2", `{"title":"Pacing","panel":"pacing","components":[{"type":"text","text":"v2"}]}`)...)
		case 8:
			// …which startup recovery settles here, in the owner's turn, with
			// a row that names no sender: it is the teammate's result.
			out = append(out, hEntry(t, "tool", "tool_result", map[string]any{"id": "send-1", "name": "send_email", "text": "teammate's interrupted send", "is_err": true}))
		}
		out = append(out, reply, summary)
		if n == 3 {
			out = append(out, hEntry(t, "user", "text", agent.TextContent{Text: "[UI submission] card=ui-form action=submit\n```json\n{\"amount\":\"12\"}\n```", SenderEmail: owner}))
			out = append(out, hEntry(t, "assistant", "text", agent.TextContent{Text: "Noted."}))
		}
	}
	return out
}

// For every participant and every window, the windowed read is exactly the
// whole-history projection restricted to the window's ids. A window never
// shows a participant an entry, a field or a redaction the whole read would
// not — including the result settled turns after its call, whose sender a
// window alone cannot name.
func TestHistoryWindowProjectsLikeTheWholeHistory(t *testing.T) {
	t.Parallel()
	s := serverFixture(t)
	ctx := context.Background()
	const owner, mate = "owner@x.com", "mate@x.com"
	conv, err := s.store.CreateConversation(ctx, owner, "multi", "generic", "", false)
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	appendEntries(t, s.store, conv.ID, multiSenderHistory(t))
	full, err := s.store.LoadHistory(ctx, conv.ID)
	if err != nil {
		t.Fatalf("load history: %v", err)
	}
	st := s.concreteStore(t)

	sawFallback := false
	for _, viewer := range []string{owner, mate} {
		whole := historyForParticipant(full, viewer, owner)
		restricted := func(start, before int64) []historyEntryResponse {
			out := []historyEntryResponse{}
			for _, e := range whole {
				if e.ID >= start && (before == 0 || e.ID < before) {
					out = append(out, e)
				}
			}
			return out
		}
		for limit := 1; limit <= len(full)+1; limit++ {
			var before int64
			for range len(full) {
				entries, page, err := loadHistoryPage(ctx, st, conv.ID, viewer, owner, historyWindowRequest{limit: limit, before: before})
				if err != nil {
					t.Fatalf("viewer %s limit %d before %d: %v", viewer, limit, before, err)
				}
				want := restricted(page.NextBefore, before)
				if len(entries) != len(want) || (len(want) > 0 && !reflect.DeepEqual(entries, want)) {
					t.Fatalf("viewer %s limit %d before %d: window differs from the whole-history projection\n got %s\nwant %s",
						viewer, limit, before, historyJSON(t, entries), historyJSON(t, want))
				}
				if w, _ := st.LoadHistoryTail(ctx, conv.ID, limit, before); w.HasMore && windowNeedsWholeHistory(w.Entries) {
					sawFallback = true
				}
				// from re-reads the same start.
				if before == 0 && page.NextBefore > 0 {
					again, againPage, err := loadHistoryPage(ctx, st, conv.ID, viewer, owner, historyWindowRequest{from: page.NextBefore})
					if err != nil || againPage.NextBefore != page.NextBefore || !reflect.DeepEqual(again, entries) {
						t.Fatalf("viewer %s: from=%d differs from the tail it re-reads (err %v)", viewer, page.NextBefore, err)
					}
				}
				if !page.HasMore {
					break
				}
				before = page.NextBefore
			}
		}
	}
	if !sawFallback {
		t.Error("no window held the result settled after its call: the whole-history fallback was never exercised")
	}

	// The case the fallback exists for, stated directly: the teammate's
	// interrupted send, in a window that begins after its call. The owner's
	// whole read does not show it (it is the teammate's trace), so neither
	// does the owner's window; the teammate's window does.
	const marker = "teammate's interrupted send"
	shows := func(viewer string) bool {
		entries, _, err := loadHistoryPage(ctx, st, conv.ID, viewer, owner, historyWindowRequest{limit: 22})
		if err != nil {
			t.Fatalf("window for %s: %v", viewer, err)
		}
		return strings.Contains(string(historyJSON(t, entries)), marker)
	}
	if shows(owner) {
		t.Error("the owner's window shows a teammate's private result")
	}
	if !shows(mate) {
		t.Error("the teammate's window lost their own result")
	}
}

// What a window leaves out still reaches the board: every card that was
// shown (for every participant), the answer a card was given, the cost of the
// turns not loaded, and where the compaction summary sits.
func TestHistoryWindowKeepsEarlierDashboardCards(t *testing.T) {
	t.Parallel()
	s := serverFixture(t)
	h := s.Routes()
	ctx := context.Background()
	const owner, mate = windowTestOwner, "mate@x.com"
	id := newRouteProbeConversationAs(t, h)
	history := multiSenderHistory(t)
	// A compaction summary between the cards and the window.
	history = append(history[:len(history):len(history)], hEntry(t, "assistant", "summary", agent.SummaryContent{Text: "private summary text"}))
	history = append(history, windowTurn(t, 50, owner)...)
	ids := appendEntries(t, s.store, id, history)
	summaryID := ids[len(ids)-6]

	read, _ := getConversation(t, h, id, "?limit=3")
	if read.Page == nil || !read.Page.HasMore || read.Page.Earlier == nil {
		t.Fatalf("page = %+v, want has_more with earlier", read.Page)
	}
	if len(read.History) != 5 {
		t.Fatalf("window holds %d entries, want the newest turn (5)", len(read.History))
	}
	for _, e := range read.History {
		if e.Type == "tool_call" && strings.Contains(string(e.Content), tools.ShowUIToolName) {
			t.Fatal("the fixture's window holds a card; this test is about cards older than it")
		}
	}
	earlier := read.Page.Earlier
	type cardRow struct {
		Type string
		Call string
	}
	rowsOf := func(e *historyEarlier) []cardRow {
		var out []cardRow
		for _, c := range e.Cards {
			var content struct {
				ID   string `json:"id"`
				Text string `json:"text"`
			}
			_ = json.Unmarshal(c.Content, &content)
			if c.Role == "user" {
				out = append(out, cardRow{Type: "answer", Call: strings.SplitN(content.Text, "\n", 2)[0]})
				continue
			}
			out = append(out, cardRow{Type: c.Type, Call: content.ID})
		}
		return out
	}
	wantRows := []cardRow{
		{"tool_call", "ui-pace"}, {"tool_result", "ui-pace"},
		{"tool_call", "ui-form"}, {"tool_result", "ui-form"},
		{"answer", "[UI submission] card=ui-form action=submit"},
		{"tool_call", "ui-pace-2"}, {"tool_result", "ui-pace-2"},
	}
	if got := rowsOf(earlier); !reflect.DeepEqual(got, wantRows) {
		t.Errorf("earlier cards = %v, want %v", got, wantRows)
	}
	if earlier.CardsTrimmed {
		t.Error("a small board was reported trimmed")
	}
	body := string(historyJSON(t, earlier))
	for _, leak := range []string{"private rows", "private summary text", "files of turn", "ui-bad", "send_email"} {
		if strings.Contains(body, leak) {
			t.Errorf("earlier carries %q: it is the outline of the history, not the history", leak)
		}
	}
	// The card specs are whole: a panel is drawn from them.
	if !strings.Contains(body, `\"panel\":\"pacing\"`) || !strings.Contains(body, `\"text\":\"v2\"`) {
		t.Errorf("earlier cards lost their specs: %s", body)
	}
	if earlier.SummaryID != summaryID {
		t.Errorf("summary_id = %d, want %d", earlier.SummaryID, summaryID)
	}
	// The owner's own six earlier turns at 0.25 and 100 prompt tokens (what a
	// teammate's turn cost is not the owner's to see, in a window or out of
	// one); cached tokens are capped at the prompt tokens, as the web's totals
	// chip caps them.
	if want := (historyTotals{CostUSD: 1.5, PromptTokens: 600, CachedTokens: 600, Turns: 6}); earlier.Totals != want {
		t.Errorf("earlier totals = %+v, want %+v", earlier.Totals, want)
	}

	// A teammate's window carries the same cards (a shown card reaches every
	// participant), the answer, no summary marker they are not shown, and
	// only their own turns' cost.
	full, err := s.store.LoadHistory(ctx, id)
	if err != nil {
		t.Fatalf("load history: %v", err)
	}
	tail, err := s.concreteStore(t).LoadHistoryTail(ctx, id, 3, 0)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	outline, err := s.concreteStore(t).LoadHistoryOutline(ctx, id, tail.StartID)
	if err != nil {
		t.Fatalf("outline: %v", err)
	}
	forMate := earlierOf(outline, mate, owner)
	if got := rowsOf(forMate); !reflect.DeepEqual(got, wantRows) {
		t.Errorf("a teammate's earlier cards = %v, want %v", got, wantRows)
	}
	if forMate.SummaryID != 0 {
		t.Error("a teammate was told where the owner's summary sits")
	}
	if want := (historyTotals{CostUSD: 1.5, PromptTokens: 600, CachedTokens: 600, Turns: 6}); forMate.Totals != want {
		t.Errorf("a teammate's earlier totals = %+v, want their own six turns %+v", forMate.Totals, want)
	}
	// Results never name their sender to a participant.
	for _, c := range forMate.Cards {
		if c.Type == "tool_result" && strings.Contains(string(c.Content), "sender_email") {
			t.Errorf("card result %d names its sender", c.ID)
		}
	}

	// The outline is the whole-history projection of the same entries: every
	// card entry it returns is byte-identical to that entry in a whole read.
	for _, viewer := range []string{owner, mate} {
		whole := make(map[int64]historyEntryResponse)
		for _, e := range historyForParticipant(full, viewer, owner) {
			whole[e.ID] = e
		}
		for _, c := range earlierOf(outline, viewer, owner).Cards {
			if want, ok := whole[c.ID]; !ok || !reflect.DeepEqual(c, want) {
				t.Errorf("viewer %s: earlier card %d = %s, the whole read has %s", viewer, c.ID, historyJSON(t, c), historyJSON(t, want))
			}
		}
	}
}

// A board that a patrol redrew thousands of times stays bounded: the oldest
// replaced versions are left out, and every card nothing has replaced stays.
func TestEarlierCardsAreBoundedByDroppingReplacedVersions(t *testing.T) {
	t.Parallel()
	pad := strings.Repeat("x", 4<<10)
	var outline []agent.HistoryEntry
	id := int64(0)
	add := func(e agent.HistoryEntry) {
		id++
		e.ID = id
		outline = append(outline, e)
	}
	const versions = 400 // 400 × ~4 KiB is well past the bound
	for v := range versions {
		add(hEntry(t, "user", "text", agent.TextContent{Text: "refresh"}))
		call := fmt.Sprintf("ui-%d", v)
		add(hEntry(t, "assistant", "tool_call", agent.ToolCallContent{ID: call, Name: tools.ShowUIToolName,
			Input: fmt.Sprintf(`{"title":"Pacing","panel":"pacing","components":[{"type":"text","text":"v%d %s"}]}`, v, pad)}))
		add(hEntry(t, "tool", "tool_result", agent.ToolResultContent{ID: call, Name: tools.ShowUIToolName, Text: "UI_DISPLAYED card_id=" + call}))
	}
	// One early display card nothing ever replaced.
	add(hEntry(t, "assistant", "tool_call", agent.ToolCallContent{ID: "ui-keep", Name: tools.ShowUIToolName, Input: `{"title":"Notes","components":[{"type":"text","text":"keep me"}]}`}))
	add(hEntry(t, "tool", "tool_result", agent.ToolResultContent{ID: "ui-keep", Name: tools.ShowUIToolName, Text: "UI_DISPLAYED card_id=ui-keep"}))

	earlier := earlierOf(outline, "owner@x.com", "owner@x.com")
	if !earlier.CardsTrimmed {
		t.Fatal("an oversized board was not trimmed")
	}
	size, calls := 0, map[string]bool{}
	for _, c := range earlier.Cards {
		size += len(c.Content)
		var content struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(c.Content, &content)
		calls[c.Type+" "+content.ID] = true
	}
	if size > maxEarlierCardBytes {
		t.Errorf("trimmed cards are %d bytes, over the %d bound", size, maxEarlierCardBytes)
	}
	newest := fmt.Sprintf("ui-%d", versions-1)
	for _, must := range []string{"tool_call " + newest, "tool_result " + newest, "tool_call ui-keep", "tool_result ui-keep"} {
		if !calls[must] {
			t.Errorf("trimming dropped %q, a card nothing has replaced", must)
		}
	}
	if calls["tool_call ui-0"] || calls["tool_result ui-0"] {
		t.Error("trimming kept the oldest replaced version")
	}
	// A call is never kept without its result, or the reverse.
	for key := range calls {
		typ, call, _ := strings.Cut(key, " ")
		other := "tool_result"
		if typ == "tool_result" {
			other = "tool_call"
		}
		if !calls[other+" "+call] {
			t.Errorf("card %s kept its %s without its %s", call, typ, other)
		}
	}
}

// The outline finds a card by the quoted tool name in its stored JSON; this
// holds the query's literal to the constant it stands for.
func TestHistoryOutlineMatchesTheShowUIToolName(t *testing.T) {
	t.Parallel()
	if tools.ShowUIToolName != "show_ui" {
		t.Fatalf("show_ui was renamed to %q: update the literal in store.LoadHistoryOutline", tools.ShowUIToolName)
	}
}
