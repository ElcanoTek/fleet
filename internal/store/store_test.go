package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
)

func textEntry(role, text string) agent.HistoryEntry {
	b, _ := json.Marshal(agent.TextContent{Text: text})
	return agent.HistoryEntry{Role: role, Type: "text", Content: b}
}

func summaryEntry(text string) agent.HistoryEntry {
	b, _ := json.Marshal(agent.SummaryContent{Text: text, Model: "test/model"})
	return agent.HistoryEntry{Role: "assistant", Type: "summary", Content: b}
}

func TestCreateAndGetConversation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	c, err := s.CreateConversation(ctx, "u@x.com", "hello", "victoria", "", false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if c.ID == "" {
		t.Fatal("ID empty")
	}
	if c.Persona != "victoria" {
		t.Errorf("persona: got %q", c.Persona)
	}
	if c.Pinned {
		t.Error("default pinned should be false")
	}

	got, err := s.Get(ctx, "u@x.com", c.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil || got.Title != "hello" {
		t.Errorf("Get returned %+v", got)
	}
}

func TestGet_WrongUserReturnsNil(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	c, _ := s.CreateConversation(ctx, "owner@x.com", "", "victoria", "", false)

	got, err := s.Get(ctx, "intruder@x.com", c.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != nil {
		t.Error("cross-user Get should return nil")
	}
}

func TestList_PinnedFirstThenRecent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	a, _ := s.CreateConversation(ctx, "u@x.com", "A (older)", "victoria", "", false)
	b, _ := s.CreateConversation(ctx, "u@x.com", "B (newer, unpinned)", "victoria", "", false)
	c, _ := s.CreateConversation(ctx, "u@x.com", "C (newest, unpinned)", "victoria", "", false)
	// Distinct updated_at, deterministically (a oldest → c newest), instead of
	// wall-clock sleeps between inserts.
	base := time.Now().Unix()
	for i, id := range []string{a.ID, b.ID, c.ID} {
		if _, err := s.db.ExecContext(ctx, `UPDATE conversations SET updated_at = $1 WHERE id = $2`, base-int64(10-i), id); err != nil {
			t.Fatalf("set updated_at: %v", err)
		}
	}

	if err := s.SetPinned(ctx, "u@x.com", a.ID, true); err != nil {
		t.Fatalf("SetPinned: %v", err)
	}

	list, err := s.List(ctx, "u@x.com", false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("len: got %d", len(list))
	}
	if list[0].ID != a.ID {
		t.Errorf("pinned first: want A, got %s", list[0].Title)
	}
	if list[1].ID != c.ID {
		t.Errorf("unpinned newest: want C, got %s", list[1].Title)
	}
	if list[2].ID != b.ID {
		t.Errorf("unpinned oldest: want B, got %s", list[2].Title)
	}
}

func TestAppendAndLoadHistory(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	c, _ := s.CreateConversation(ctx, "u@x.com", "", "victoria", "", false)

	entries := []agent.HistoryEntry{
		textEntry("user", "hi"),
		textEntry("assistant", "hello"),
	}
	if _, err := s.AppendHistory(ctx, c.ID, entries); err != nil {
		t.Fatalf("AppendHistory: %v", err)
	}

	got, err := s.LoadHistory(ctx, c.ID)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len: got %d", len(got))
	}

	var first agent.TextContent
	if err := json.Unmarshal(got[0].Content, &first); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if first.Text != "hi" {
		t.Errorf("first text: got %q", first.Text)
	}
}

func TestContainedPanicPair_DBAppendLoadRoundTrip(t *testing.T) {
	const (
		callID = "db-panic-call"
		secret = "Authorization: Bearer fake-db-panic-secret"
	)
	s := newTestStore(t)
	ctx := context.Background()
	conversation, err := s.CreateConversation(ctx, "u@x.com", "panic pair", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	callJSON, _ := json.Marshal(agent.ToolCallContent{ID: callID, Name: "panicking_tool", Input: `{}`})
	resultJSON, _ := json.Marshal(agent.ToolResultContent{
		ID: callID, Name: "panicking_tool",
		Text: "Tool execution failed unexpectedly. Reference: inc_0123456789abcdef0123456789abcdef.", IsErr: true,
	})
	entries := []agent.HistoryEntry{
		{Role: "assistant", Type: "tool_call", Content: callJSON},
		{Role: "tool", Type: "tool_result", Content: resultJSON},
	}
	if _, err := s.AppendHistory(ctx, conversation.ID, entries); err != nil {
		t.Fatalf("AppendHistory: %v", err)
	}
	stored, err := s.LoadHistory(ctx, conversation.ID)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	encoded, _ := json.Marshal(stored)
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("DB history leaked panic value: %s", encoded)
	}
	var calls, results int
	for _, entry := range stored {
		switch entry.Type {
		case "tool_call":
			var call agent.ToolCallContent
			if err := json.Unmarshal(entry.Content, &call); err != nil {
				t.Fatal(err)
			}
			if call.ID == callID {
				calls++
			}
		case "tool_result":
			var result agent.ToolResultContent
			if err := json.Unmarshal(entry.Content, &result); err != nil {
				t.Fatal(err)
			}
			if result.ID == callID {
				results++
				if !result.IsErr || !strings.Contains(result.Text, "inc_") {
					t.Fatalf("stored result = %+v", result)
				}
			}
		}
	}
	if calls != 1 || results != 1 {
		t.Fatalf("DB pair counts: calls=%d results=%d stored=%+v", calls, results, stored)
	}
}

func TestDelete_CascadesMessages(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	c, _ := s.CreateConversation(ctx, "u@x.com", "", "victoria", "", false)
	_, _ = s.AppendHistory(ctx, c.ID, []agent.HistoryEntry{textEntry("user", "x")})

	if err := s.Delete(ctx, "u@x.com", c.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	got, err := s.LoadHistory(ctx, c.ID)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("messages not cascaded: got %d", len(got))
	}
}

func TestSweep_TTL(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	c, _ := s.CreateConversation(ctx, "u@x.com", "", "victoria", "", false)

	// Backdate the row by 30 days.
	backdate := time.Now().Add(-30 * 24 * time.Hour).Unix()
	_, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET updated_at = $1 WHERE id = $2`, backdate, c.ID)
	if err != nil {
		t.Fatalf("backdate: %v", err)
	}

	expired, evicted, err := s.SweepExpired(ctx, 14*24*time.Hour, 100)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if expired != 1 {
		t.Errorf("expired count: got %d", expired)
	}
	if evicted != 0 {
		t.Errorf("evicted count: got %d", evicted)
	}
}

func TestSweep_TTLRespectsPin(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	c, _ := s.CreateConversation(ctx, "u@x.com", "", "victoria", "", false)
	_ = s.SetPinned(ctx, "u@x.com", c.ID, true)

	// Backdate AFTER pin toggle, since SetPinned updates updated_at.
	backdate := time.Now().Add(-30 * 24 * time.Hour).Unix()
	_, _ = s.db.ExecContext(ctx,
		`UPDATE conversations SET updated_at = $1 WHERE id = $2`, backdate, c.ID)

	expired, _, err := s.SweepExpired(ctx, 14*24*time.Hour, 100)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if expired != 0 {
		t.Error("pinned conversation was swept")
	}
	got, _ := s.Get(ctx, "u@x.com", c.ID)
	if got == nil {
		t.Fatal("pinned conversation deleted")
	}
}

func TestSweep_UnpinnedCap(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Create 6 unpinned conversations. Cap at 3 — expect 3 evicted. Assign
	// distinct, increasing updated_at deterministically (no sleeps) so the cap
	// evicts the oldest 3.
	ids := make([]string, 6)
	for i := 0; i < 6; i++ {
		conv, _ := s.CreateConversation(ctx, "u@x.com", "", "victoria", "", false)
		ids[i] = conv.ID
	}
	base := time.Now().Unix()
	for i, id := range ids {
		if _, err := s.db.ExecContext(ctx, `UPDATE conversations SET updated_at = $1 WHERE id = $2`, base-int64(60-i), id); err != nil {
			t.Fatalf("set updated_at: %v", err)
		}
	}

	_, evicted, err := s.SweepExpired(ctx, 14*24*time.Hour, 3)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if evicted != 3 {
		t.Errorf("evicted: got %d want 3", evicted)
	}

	remaining, _ := s.List(ctx, "u@x.com", false)
	if len(remaining) != 3 {
		t.Errorf("remaining: got %d want 3", len(remaining))
	}
}

// TestSweep_UnpinnedCapPerUser pins that the single-statement cap eviction
// partitions by user: each user is trimmed to the cap against their OWN
// newest-first ordering, and a user under the cap is untouched. (The previous
// shape ran one DELETE per overflowing user, so per-user independence was
// structural; with ROW_NUMBER() it is the partition clause's job.)
func TestSweep_UnpinnedCapPerUser(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// a@x.com: 5 conversations (2 over a cap of 3)
	// b@x.com: 4 conversations (1 over)
	// c@x.com: 2 conversations (under the cap — must survive intact)
	counts := map[string]int{"a@x.com": 5, "b@x.com": 4, "c@x.com": 2}
	base := time.Now().Unix()
	kept := map[string][]string{}
	for user, n := range counts {
		for i := 0; i < n; i++ {
			conv, err := s.CreateConversation(ctx, user, "", "victoria", "", false)
			if err != nil {
				t.Fatalf("CreateConversation: %v", err)
			}
			// Older rows first: index 0 is the oldest, so the last 3 created
			// are the ones the cap keeps.
			if _, err := s.db.ExecContext(ctx, `UPDATE conversations SET updated_at = $1 WHERE id = $2`, base-int64(60-i), conv.ID); err != nil {
				t.Fatalf("set updated_at: %v", err)
			}
			if i >= n-3 {
				kept[user] = append(kept[user], conv.ID)
			}
		}
	}

	_, evicted, err := s.SweepExpired(ctx, 14*24*time.Hour, 3)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if evicted != 3 { // 2 from a@, 1 from b@, 0 from c@
		t.Errorf("evicted: got %d want 3", evicted)
	}

	for user, want := range map[string]int{"a@x.com": 3, "b@x.com": 3, "c@x.com": 2} {
		remaining, err := s.List(ctx, user, false)
		if err != nil {
			t.Fatalf("List(%s): %v", user, err)
		}
		if len(remaining) != want {
			t.Errorf("%s remaining: got %d want %d", user, len(remaining), want)
		}
		survived := map[string]bool{}
		for _, c := range remaining {
			survived[c.ID] = true
		}
		for _, id := range kept[user] {
			if !survived[id] {
				t.Errorf("%s: newest conversation %s was evicted", user, id)
			}
		}
	}
}

func TestSetPinned_WrongUser(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	c, _ := s.CreateConversation(ctx, "owner@x.com", "", "victoria", "", false)

	err := s.SetPinned(ctx, "intruder@x.com", c.ID, true)
	if err == nil {
		t.Fatal("expected error for wrong user")
	}
}

// TestReplaceSummary_HappyPath — first call inserts; second call
// deletes the prior summary and inserts the new one. Replace
// semantics keep summary-of-summary chains from accumulating.
func TestReplaceSummary_HappyPath(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	c, _ := s.CreateConversation(ctx, "u@x.com", "", "victoria", "", false)
	_, _ = s.AppendHistory(ctx, c.ID, []agent.HistoryEntry{
		textEntry("user", "hi"),
		textEntry("assistant", "hello"),
	})

	if err := s.ReplaceSummary(ctx, "u@x.com", c.ID, summaryEntry("first")); err != nil {
		t.Fatalf("first ReplaceSummary: %v", err)
	}
	hist, _ := s.LoadHistory(ctx, c.ID)
	summaries := countByType(hist, "summary")
	if summaries != 1 {
		t.Fatalf("expected 1 summary after first call, got %d", summaries)
	}

	if err := s.ReplaceSummary(ctx, "u@x.com", c.ID, summaryEntry("second")); err != nil {
		t.Fatalf("second ReplaceSummary: %v", err)
	}
	hist, _ = s.LoadHistory(ctx, c.ID)
	summaries = countByType(hist, "summary")
	if summaries != 1 {
		t.Fatalf("expected 1 summary after replace, got %d (replace semantics broken)", summaries)
	}

	// And it must be the *new* one.
	for _, e := range hist {
		if e.Type != "summary" {
			continue
		}
		var c agent.SummaryContent
		_ = json.Unmarshal(e.Content, &c)
		if c.Text != "second" {
			t.Errorf("expected latest summary text 'second', got %q", c.Text)
		}
	}

	// Pre-summary user/assistant turns must remain in the DB — the
	// LLM-context boundary is enforced by replayHistory, not by
	// deletion.
	if countByType(hist, "text") != 2 {
		t.Errorf("expected pre-summary text turns to remain in DB, got %d", countByType(hist, "text"))
	}
}

// TestReplaceSummary_RejectsWrongType — defensive guard to keep a
// caller from using ReplaceSummary as a backdoor delete-by-type for
// other entry kinds.
func TestReplaceSummary_RejectsWrongType(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	c, _ := s.CreateConversation(ctx, "u@x.com", "", "victoria", "", false)

	bad := textEntry("assistant", "not a summary")
	if err := s.ReplaceSummary(ctx, "u@x.com", c.ID, bad); err == nil {
		t.Fatal("expected error for non-summary entry type")
	}
}

// TestReplaceSummary_WrongUser — owner-scoped check.
func TestReplaceSummary_WrongUser(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	c, _ := s.CreateConversation(ctx, "owner@x.com", "", "victoria", "", false)

	if err := s.ReplaceSummary(ctx, "intruder@x.com", c.ID, summaryEntry("x")); err == nil {
		t.Fatal("expected error for wrong user")
	}
}

func countByType(entries []agent.HistoryEntry, typ string) int {
	n := 0
	for _, e := range entries {
		if e.Type == typ {
			n++
		}
	}
	return n
}
