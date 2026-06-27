package store

import (
	"context"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// TestSearchConversations_TitleAndContent covers the core FTS contract: matches
// on a conversation title AND on message content, scoped to the owner, with a
// <mark>-highlighted preview.
func TestSearchConversations_TitleAndContent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	c, err := s.CreateConversation(ctx, "alice@example.com", "Python async patterns", "assistant", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if err := s.AppendHistory(ctx, c.ID, []agent.HistoryEntry{
		textEntry("user", "How do goroutines compare to async functions?"),
		textEntry("assistant", "A goroutine is a lightweight thread scheduled by the Go runtime."),
	}); err != nil {
		t.Fatalf("AppendHistory: %v", err)
	}
	// A second, unrelated conversation that must NOT match "goroutine".
	if _, err := s.CreateConversation(ctx, "alice@example.com", "Lunch plans", "assistant", "", false); err != nil {
		t.Fatalf("CreateConversation 2: %v", err)
	}

	// Title match.
	res, total, err := s.SearchConversations(ctx, "alice@example.com", "python", 20, 0)
	if err != nil {
		t.Fatalf("SearchConversations(title): %v", err)
	}
	if total != 1 || len(res) != 1 || res[0].ConversationID != c.ID {
		t.Fatalf("title search: total=%d res=%+v, want the python conversation", total, res)
	}

	// Content match with a highlighted preview.
	res, total, err = s.SearchConversations(ctx, "alice@example.com", "goroutine", 20, 0)
	if err != nil {
		t.Fatalf("SearchConversations(content): %v", err)
	}
	if total != 1 || len(res) != 1 || res[0].ConversationID != c.ID {
		t.Fatalf("content search: total=%d res=%+v, want the python conversation", total, res)
	}
	if !strings.Contains(strings.ToLower(res[0].Preview), "<mark>goroutine") {
		t.Errorf("preview missing <mark> highlight: %q", res[0].Preview)
	}
}

// TestSearchConversations_ScopedByUser ensures a user never sees another user's
// conversations in search results.
func TestSearchConversations_ScopedByUser(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	mine, err := s.CreateConversation(ctx, "alice@example.com", "shared secret recipe", "assistant", "", false)
	if err != nil {
		t.Fatalf("CreateConversation mine: %v", err)
	}
	if _, err := s.CreateConversation(ctx, "bob@example.com", "shared secret plan", "assistant", "", false); err != nil {
		t.Fatalf("CreateConversation other: %v", err)
	}

	res, total, err := s.SearchConversations(ctx, "alice@example.com", "shared secret", 20, 0)
	if err != nil {
		t.Fatalf("SearchConversations: %v", err)
	}
	if total != 1 || len(res) != 1 || res[0].ConversationID != mine.ID {
		t.Fatalf("scoped search: total=%d res=%+v, want only alice's conversation", total, res)
	}
}

// TestSearchConversations_EmptyQuery returns nothing rather than every row.
func TestSearchConversations_EmptyQuery(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.CreateConversation(ctx, "a@e.com", "anything", "assistant", "", false); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	res, total, err := s.SearchConversations(ctx, "a@e.com", "   ", 20, 0)
	if err != nil {
		t.Fatalf("SearchConversations: %v", err)
	}
	if total != 0 || len(res) != 0 {
		t.Errorf("empty query: total=%d len=%d, want 0/0", total, len(res))
	}
}

// TestBackfillSearchContent populates message_search_content for messages that
// predate FTS — simulated by deleting the rows AppendHistory wrote, then
// re-running the backfill.
func TestBackfillSearchContent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	c, err := s.CreateConversation(ctx, "alice@example.com", "untitled", "assistant", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if err := s.AppendHistory(ctx, c.ID, []agent.HistoryEntry{
		textEntry("user", "tell me about hippopotamus migration"),
	}); err != nil {
		t.Fatalf("AppendHistory: %v", err)
	}

	// Simulate pre-FTS data: wipe the extracted rows so only raw messages remain.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM message_search_content`); err != nil {
		t.Fatalf("delete search content: %v", err)
	}
	if res, _, _ := s.SearchConversations(ctx, "alice@example.com", "hippopotamus", 20, 0); len(res) != 0 {
		t.Fatalf("expected no content match before backfill, got %d", len(res))
	}

	n, err := s.BackfillSearchContent(ctx)
	if err != nil {
		t.Fatalf("BackfillSearchContent: %v", err)
	}
	if n != 1 {
		t.Errorf("backfill inserted %d rows, want 1", n)
	}
	res, total, err := s.SearchConversations(ctx, "alice@example.com", "hippopotamus", 20, 0)
	if err != nil {
		t.Fatalf("SearchConversations after backfill: %v", err)
	}
	if total != 1 || len(res) != 1 {
		t.Errorf("after backfill: total=%d len=%d, want 1/1", total, len(res))
	}

	// Idempotent: a second backfill inserts nothing.
	if n2, err := s.BackfillSearchContent(ctx); err != nil || n2 != 0 {
		t.Errorf("second backfill: n=%d err=%v, want 0/nil", n2, err)
	}
}

// TestSearchDisabledSkipsIndexing verifies SetSearchEnabled(false) stops
// AppendHistory from populating the search table and no-ops the backfill.
func TestSearchDisabledSkipsIndexing(t *testing.T) {
	s := newTestStore(t)
	s.SetSearchEnabled(false)
	ctx := context.Background()

	c, err := s.CreateConversation(ctx, "alice@example.com", "title only", "assistant", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if err := s.AppendHistory(ctx, c.ID, []agent.HistoryEntry{
		textEntry("user", "secretkeyword in the body"),
	}); err != nil {
		t.Fatalf("AppendHistory: %v", err)
	}

	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM message_search_content`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("search-disabled write left %d search rows, want 0", count)
	}
	if n, err := s.BackfillSearchContent(ctx); err != nil || n != 0 {
		t.Errorf("disabled backfill: n=%d err=%v, want 0/nil", n, err)
	}
}
