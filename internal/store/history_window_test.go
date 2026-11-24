package store

// The windowed history reads (docs/CHAT-HISTORY-WINDOW.md): a suffix of the
// history that only ever begins where a turn opens, the cursor that pages back
// through it, and the outline of what a window leaves out.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
)

func windowEntry(t *testing.T, role, typ string, v any) agent.HistoryEntry {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return agent.HistoryEntry{Role: role, Type: typ, Content: b}
}

// seedWindowTurns appends `turns` turns of five entries each (a prompt, a tool
// call and its result, a reply, the turn summary) and returns each turn's
// entry ids. Turn numbering starts at 0.
func seedWindowTurns(t *testing.T, s *Store, convID string, turns int) [][]int64 {
	t.Helper()
	out := make([][]int64, 0, turns)
	for turn := range turns {
		call := fmt.Sprintf("call-%d", turn)
		ids, err := s.AppendHistory(context.Background(), convID, []agent.HistoryEntry{
			windowEntry(t, "user", "text", agent.TextContent{Text: fmt.Sprintf("prompt %d", turn)}),
			windowEntry(t, "assistant", "tool_call", agent.ToolCallContent{ID: call, Name: "run_python", Input: "{}"}),
			windowEntry(t, "tool", "tool_result", agent.ToolResultContent{ID: call, Name: "run_python", Text: strings.Repeat("row\n", 2000)}),
			windowEntry(t, "assistant", "text", agent.TextContent{Text: fmt.Sprintf("reply %d", turn)}),
			windowEntry(t, "assistant", "turn_summary", agent.TurnSummaryContent{CostUSD: 0.5, PromptTokens: 100}),
		})
		if err != nil {
			t.Fatalf("append turn %d: %v", turn, err)
		}
		out = append(out, ids)
	}
	return out
}

func newWindowConversation(t *testing.T, s *Store) string {
	t.Helper()
	conv, err := s.CreateConversation(context.Background(), "owner@x.com", "t", "generic", "", false)
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	return conv.ID
}

func entryIDs(entries []agent.HistoryEntry) []int64 {
	ids := make([]int64, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	return ids
}

func flatten(turns [][]int64) []int64 {
	var out []int64
	for _, ids := range turns {
		out = append(out, ids...)
	}
	return out
}

func sameIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A tail never splits a turn: the limit lands partway through a turn and the
// window is widened back to that turn's opening message.
func TestLoadHistoryTailStartsWhereATurnOpens(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	convID := newWindowConversation(t, s)
	turns := seedWindowTurns(t, s, convID, 6)

	cases := []struct {
		limit     int
		wantTurns int // how many of the newest turns the window holds
	}{
		{limit: 1, wantTurns: 1},
		{limit: 5, wantTurns: 1},
		{limit: 6, wantTurns: 2}, // one entry into the turn before: all of it
		{limit: 12, wantTurns: 3},
		{limit: 30, wantTurns: 6},
		{limit: 500, wantTurns: 6},
	}
	for _, c := range cases {
		w, err := s.LoadHistoryTail(ctx, convID, c.limit, 0)
		if err != nil {
			t.Fatalf("limit %d: %v", c.limit, err)
		}
		want := flatten(turns[len(turns)-c.wantTurns:])
		if got := entryIDs(w.Entries); !sameIDs(got, want) {
			t.Errorf("limit %d: entries %v, want %v", c.limit, got, want)
		}
		if w.StartID != want[0] {
			t.Errorf("limit %d: StartID %d, want %d", c.limit, w.StartID, want[0])
		}
		if wantMore := c.wantTurns < len(turns); w.HasMore != wantMore {
			t.Errorf("limit %d: HasMore %v, want %v", c.limit, w.HasMore, wantMore)
		}
	}
}

// Paging back with the cursor a window returns covers the whole history once,
// in order, with no entry repeated or skipped — and agrees with LoadHistory.
func TestLoadHistoryTailPagesBackWithoutGapsOrRepeats(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	convID := newWindowConversation(t, s)
	seedWindowTurns(t, s, convID, 9)

	full, err := s.LoadHistory(ctx, convID)
	if err != nil {
		t.Fatalf("load history: %v", err)
	}
	var (
		pages  [][]agent.HistoryEntry
		before int64
	)
	for range 20 {
		w, err := s.LoadHistoryTail(ctx, convID, 7, before)
		if err != nil {
			t.Fatalf("page before %d: %v", before, err)
		}
		if len(w.Entries) == 0 {
			t.Fatalf("page before %d is empty", before)
		}
		if w.Entries[0].ID != w.StartID {
			t.Fatalf("page before %d starts at %d, StartID says %d", before, w.Entries[0].ID, w.StartID)
		}
		pages = append([][]agent.HistoryEntry{w.Entries}, pages...)
		if !w.HasMore {
			break
		}
		before = w.StartID
	}
	var got []agent.HistoryEntry
	for _, p := range pages {
		got = append(got, p...)
	}
	if len(got) != len(full) {
		t.Fatalf("pages hold %d entries, the history %d", len(got), len(full))
	}
	for i := range full {
		if got[i].ID != full[i].ID || string(got[i].Content) != string(full[i].Content) || got[i].Type != full[i].Type || got[i].Role != full[i].Role {
			t.Fatalf("entry %d: paged %+v, stored %+v", i, got[i], full[i])
		}
	}
	// 9 turns of 5 entries, 7 at a time: two turns a page, so more than one page.
	if len(pages) < 4 {
		t.Errorf("history came back in %d pages; the limit was not applied", len(pages))
	}
}

// A message steered into a running turn does not open a turn, so a window
// never begins at one.
func TestLoadHistoryTailDoesNotStartAtASteeredMessage(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	convID := newWindowConversation(t, s)
	seedWindowTurns(t, s, convID, 1)
	ids, err := s.AppendHistory(ctx, convID, []agent.HistoryEntry{
		windowEntry(t, "user", "text", agent.TextContent{Text: "start"}),
		windowEntry(t, "assistant", "tool_call", agent.ToolCallContent{ID: "c1", Name: "bash", Input: "{}"}),
		windowEntry(t, "user", "text", agent.TextContent{Text: "also check Tuesday", Steered: true}),
		windowEntry(t, "tool", "tool_result", agent.ToolResultContent{ID: "c1", Name: "bash", Text: "ok"}),
		windowEntry(t, "assistant", "text", agent.TextContent{Text: "done"}),
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	// Three entries reach back exactly to the steered message.
	w, err := s.LoadHistoryTail(ctx, convID, 3, 0)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	if w.StartID != ids[0] || !sameIDs(entryIDs(w.Entries), ids) {
		t.Errorf("window %v starts at %d; want the whole turn %v", entryIDs(w.Entries), w.StartID, ids)
	}
	if !w.HasMore {
		t.Error("the earlier turn was not reported")
	}
}

// A history no turn opens in (entries written before any user message) comes
// back whole rather than cut at an arbitrary entry.
func TestLoadHistoryTailWithNoTurnStartReturnsEverything(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	convID := newWindowConversation(t, s)
	batch := make([]agent.HistoryEntry, 0, 600)
	for i := range 600 {
		batch = append(batch, windowEntry(t, "assistant", "text", agent.TextContent{Text: fmt.Sprint(i)}))
	}
	ids, err := s.AppendHistory(ctx, convID, batch)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	w, err := s.LoadHistoryTail(ctx, convID, 10, 0)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	if len(w.Entries) != len(ids) || w.StartID != ids[0] || w.HasMore {
		t.Errorf("window holds %d of %d entries, StartID %d (first %d), HasMore %v", len(w.Entries), len(ids), w.StartID, ids[0], w.HasMore)
	}
}

func TestLoadHistoryTailOfAnEmptyConversation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	convID := newWindowConversation(t, s)
	w, err := s.LoadHistoryTail(ctx, convID, 60, 0)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	if len(w.Entries) != 0 || w.StartID != 0 || w.HasMore {
		t.Errorf("empty conversation gave %+v", w)
	}
	// A cursor older than every entry is an empty page, not an error.
	turns := seedWindowTurns(t, s, convID, 1)
	w, err = s.LoadHistoryTail(ctx, convID, 60, turns[0][0])
	if err != nil {
		t.Fatalf("tail before the first entry: %v", err)
	}
	if len(w.Entries) != 0 || w.HasMore {
		t.Errorf("page before the first entry gave %+v", w)
	}
}

// One conversation's window never holds another's entries, though message ids
// are drawn from one sequence and interleave.
func TestLoadHistoryTailIsScopedToItsConversation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	a, b := newWindowConversation(t, s), newWindowConversation(t, s)
	aIDs := make([]int64, 0, 20)
	for range 4 {
		aIDs = append(aIDs, flatten(seedWindowTurns(t, s, a, 1))...)
		seedWindowTurns(t, s, b, 1)
	}
	w, err := s.LoadHistoryTail(ctx, a, 500, 0)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	if !sameIDs(entryIDs(w.Entries), aIDs) {
		t.Errorf("window %v, want only conversation a's %v", entryIDs(w.Entries), aIDs)
	}
	// And the outline of a is of a alone.
	outline, err := s.LoadHistoryOutline(ctx, a, aIDs[len(aIDs)-1]+1_000)
	if err != nil {
		t.Fatalf("outline: %v", err)
	}
	inA := make(map[int64]bool, len(aIDs))
	for _, id := range aIDs {
		inA[id] = true
	}
	for _, e := range outline {
		if !inA[e.ID] {
			t.Errorf("outline of a holds entry %d of another conversation", e.ID)
		}
	}
}

// LoadHistoryFrom re-reads a held window. A caller cannot begin one partway
// through a turn: the id is moved back to the turn's opening message.
func TestLoadHistoryFromSnapsToTheTurnStart(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	convID := newWindowConversation(t, s)
	turns := seedWindowTurns(t, s, convID, 4)

	for _, from := range []int64{turns[2][0], turns[2][2], turns[2][4]} {
		w, err := s.LoadHistoryFrom(ctx, convID, from)
		if err != nil {
			t.Fatalf("from %d: %v", from, err)
		}
		want := flatten(turns[2:])
		if !sameIDs(entryIDs(w.Entries), want) || w.StartID != turns[2][0] || !w.HasMore {
			t.Errorf("from %d: entries %v (StartID %d, HasMore %v), want %v from %d", from, entryIDs(w.Entries), w.StartID, w.HasMore, want, turns[2][0])
		}
	}
	// From the first entry: everything, and nothing older.
	w, err := s.LoadHistoryFrom(ctx, convID, turns[0][0])
	if err != nil {
		t.Fatalf("from the first entry: %v", err)
	}
	if !sameIDs(entryIDs(w.Entries), flatten(turns)) || w.HasMore {
		t.Errorf("from the first entry: %v, HasMore %v", entryIDs(w.Entries), w.HasMore)
	}
	// An id past the newest entry reads the newest turn.
	w, err = s.LoadHistoryFrom(ctx, convID, turns[3][4]+1_000_000)
	if err != nil {
		t.Fatalf("from past the end: %v", err)
	}
	if w.StartID != turns[3][0] {
		t.Errorf("from past the end: StartID %d, want the newest turn's %d", w.StartID, turns[3][0])
	}
	// Entries appended after a window was read are part of its re-read.
	more := seedWindowTurns(t, s, convID, 1)
	w, err = s.LoadHistoryFrom(ctx, convID, turns[3][0])
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if want := append(append([]int64{}, turns[3]...), more[0]...); !sameIDs(entryIDs(w.Entries), want) {
		t.Errorf("re-read %v, want %v", entryIDs(w.Entries), want)
	}
}

// The outline holds the user messages, turn summaries and show_ui cards older
// than the cursor, and none of the tool output, reasoning or replies. A
// compaction summary is marked without its text.
func TestLoadHistoryOutline(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	convID := newWindowConversation(t, s)
	big := strings.Repeat("secret tool output mentioning show_ui and \"show_ui\" in prose\n", 500)
	ids, err := s.AppendHistory(ctx, convID, []agent.HistoryEntry{
		windowEntry(t, "user", "text", agent.TextContent{Text: "draw the board"}),                                              // 0 kept
		windowEntry(t, "assistant", "reasoning", agent.TextContent{Text: "thinking about show_ui"}),                            // 1
		windowEntry(t, "assistant", "tool_call", agent.ToolCallContent{ID: "q1", Name: "run_python", Input: `{"code":"x"}`}),   // 2
		windowEntry(t, "tool", "tool_result", agent.ToolResultContent{ID: "q1", Name: "run_python", Text: big}),                // 3
		windowEntry(t, "assistant", "tool_call", agent.ToolCallContent{ID: "ui1", Name: "show_ui", Input: `{"title":"Pace"}`}), // 4 kept
		windowEntry(t, "tool", "tool_result", agent.ToolResultContent{ID: "ui1", Name: "show_ui", Text: "UI_DISPLAYED"}),       // 5 kept
		windowEntry(t, "assistant", "text", agent.TextContent{Text: "Here is the board, via show_ui."}),                        // 6
		windowEntry(t, "assistant", "turn_summary", agent.TurnSummaryContent{CostUSD: 1}),                                      // 7 kept
		windowEntry(t, "assistant", "summary", agent.SummaryContent{Text: "a long private summary"}),                           // 8 kept, emptied
		windowEntry(t, "user", "text", agent.TextContent{Text: "newer"}),                                                       // 9 (the cursor)
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	outline, err := s.LoadHistoryOutline(ctx, convID, ids[9])
	if err != nil {
		t.Fatalf("outline: %v", err)
	}
	want := []int64{ids[0], ids[4], ids[5], ids[7], ids[8]}
	if !sameIDs(entryIDs(outline), want) {
		t.Fatalf("outline %v, want %v", entryIDs(outline), want)
	}
	for _, e := range outline {
		if strings.Contains(string(e.Content), "secret tool output") {
			t.Errorf("outline entry %d carries tool output", e.ID)
		}
		if e.InjectedContext != "" {
			t.Errorf("outline entry %d carries injected context", e.ID)
		}
	}
	if last := outline[len(outline)-1]; last.Type != "summary" || string(last.Content) != "{}" {
		t.Errorf("summary came back as %s %s; want it marked without its text", last.Type, last.Content)
	}
	// Nothing is older than the first entry.
	if got, err := s.LoadHistoryOutline(ctx, convID, ids[0]); err != nil || len(got) != 0 {
		t.Errorf("outline before the first entry = %v, %v", got, err)
	}
	if got, err := s.LoadHistoryOutline(ctx, convID, 0); err != nil || got != nil {
		t.Errorf("outline with no cursor = %v, %v", got, err)
	}
}
