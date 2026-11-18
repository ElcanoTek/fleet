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
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()

	c, err := s.CreateConversation(ctx, "alice@example.com", "Python async patterns", "assistant", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if _, err := s.AppendHistory(ctx, c.ID, []agent.HistoryEntry{
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
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()

	c, err := s.CreateConversation(ctx, "alice@example.com", "untitled", "assistant", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if _, err := s.AppendHistory(ctx, c.ID, []agent.HistoryEntry{
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
	t.Parallel()

	s := newTestStore(t)
	s.SetSearchEnabled(false)
	ctx := context.Background()

	c, err := s.CreateConversation(ctx, "alice@example.com", "title only", "assistant", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if _, err := s.AppendHistory(ctx, c.ID, []agent.HistoryEntry{
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

// searchIDs runs a search for email and returns the matched conversation ids
// mapped to the owner email each result carried ("" for the caller's own).
func searchIDs(t *testing.T, s *Store, email, query string) map[string]string {
	t.Helper()
	res, total, err := s.SearchConversations(context.Background(), email, query, 50, 0)
	if err != nil {
		t.Fatalf("SearchConversations(%s, %q): %v", email, query, err)
	}
	if total != len(res) {
		t.Fatalf("SearchConversations(%s, %q): total=%d but %d results", email, query, total, len(res))
	}
	out := map[string]string{}
	for _, r := range res {
		out[r.ConversationID] = r.OwnerEmail
	}
	return out
}

// A chat shared with the caller's team is found by its title and by what was
// said in it — and by nobody the share does not reach. The gate is the one
// the Team chats list uses, so each refusal here is one that list makes too.
func TestSearchConversations_FindsChatsSharedWithTheCaller(t *testing.T) {
	t.Parallel()

	f := newTeamFixture(t)
	c := f.sharedChat(t, "alice@x.com", f.project.ID, "Zeppelin budget")

	// A teammate: by title, and by message text, with the owner named.
	for _, q := range []string{"zeppelin", "spread"} {
		got := searchIDs(t, f.s, "bob@x.com", q)
		if owner, ok := got[c.ID]; !ok || owner != "alice@x.com" {
			t.Errorf("teammate search %q = %v, want %s owned by alice", q, got, c.ID)
		}
	}
	// The owner still finds it, as their own (no owner email on the result).
	if owner, ok := searchIDs(t, f.s, "alice@x.com", "spread")[c.ID]; !ok || owner != "" {
		t.Errorf("owner search: found=%v owner=%q, want found with no owner email", ok, owner)
	}
	// Another team, and a person on no team: nothing.
	for _, email := range []string{"dana@x.com", "carol@x.com"} {
		for _, q := range []string{"zeppelin", "spread"} {
			if got := searchIDs(t, f.s, email, q); len(got) != 0 {
				t.Errorf("%s search %q = %v, want nothing", email, q, got)
			}
		}
	}

	// A private chat of alice's with the same words stays private.
	private, err := f.s.CreateConversation(f.ctx, "alice@x.com", "Zeppelin private notes", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if _, ok := searchIDs(t, f.s, "bob@x.com", "zeppelin")[private.ID]; ok {
		t.Error("a teammate found a chat that was never shared")
	}

	// Stop sharing: gone from the teammate's search on the next query.
	if _, err := f.s.SetConversationTeamVisible(f.ctx, "alice@x.com", c.ID, false); err != nil {
		t.Fatalf("SetConversationTeamVisible(false): %v", err)
	}
	if got := searchIDs(t, f.s, "bob@x.com", "spread"); len(got) != 0 {
		t.Errorf("after unsharing, teammate search = %v, want nothing", got)
	}
}

// In a shared chat only what a teammate may read is searched: user and
// assistant text. A tool call is never indexed, and a compaction summary —
// which the owner can search, but a team reader gets as a content-free
// boundary — is not matched or quoted for anyone but the owner.
func TestSearchConversations_SharedChatsSearchOnlyWhatATeammateReads(t *testing.T) {
	t.Parallel()

	f := newTeamFixture(t)
	c := f.sharedChat(t, "alice@x.com", f.project.ID, "Rates desk")
	if _, err := f.s.AppendHistory(f.ctx, c.ID, []agent.HistoryEntry{
		{Role: "assistant", Type: "summary", Content: []byte(`{"text":"earlier we discussed the marmalade forecast"}`)},
	}); err != nil {
		t.Fatalf("AppendHistory: %v", err)
	}

	if _, ok := searchIDs(t, f.s, "alice@x.com", "marmalade")[c.ID]; !ok {
		t.Fatal("the owner should find their own chat by its summary text")
	}
	if got := searchIDs(t, f.s, "bob@x.com", "marmalade"); len(got) != 0 {
		t.Errorf("a teammate matched summary text they cannot read: %v", got)
	}
	// The tool call's arguments (sharedChat appends one) are not searchable.
	if got := searchIDs(t, f.s, "bob@x.com", "passwd"); len(got) != 0 {
		t.Errorf("a teammate matched a tool call: %v", got)
	}

	// A title match must not quote the summary in its preview either.
	res, _, err := f.s.SearchConversations(f.ctx, "bob@x.com", "rates", 20, 0)
	if err != nil || len(res) != 1 {
		t.Fatalf("title search: res=%+v err=%v", res, err)
	}
	if strings.Contains(strings.ToLower(res[0].Preview), "marmalade") {
		t.Errorf("preview quotes text a teammate cannot read: %q", res[0].Preview)
	}
}

// Archived, sealed and Multiplayer-excluded chats follow the shared list's
// rule exactly: what it would not list, search does not find.
func TestSearchConversations_SharedSetFollowsTheSharedList(t *testing.T) {
	t.Parallel()

	f := newTeamFixture(t)

	// Archived by its owner: out of the shared list, so out of search — while
	// the owner still finds their own archived chat.
	archived := f.sharedChat(t, "alice@x.com", f.project.ID, "Quokka archive")
	if err := f.s.SetArchived(f.ctx, "alice@x.com", archived.ID, true); err != nil {
		t.Fatalf("SetArchived: %v", err)
	}
	if got := searchIDs(t, f.s, "bob@x.com", "quokka"); len(got) != 0 {
		t.Errorf("teammate found an archived shared chat: %v", got)
	}
	if _, ok := searchIDs(t, f.s, "alice@x.com", "quokka")[archived.ID]; !ok {
		t.Error("the owner should still find their own archived chat")
	}

	// Multiplayer with no team stamp: an explicitly added participant finds
	// it; someone the owner removed does not.
	mp, err := f.s.CreateConversation(f.ctx, "dana@x.com", "Narwhal war room", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if _, err := f.s.AppendHistory(f.ctx, mp.ID, []agent.HistoryEntry{textEntry("user", "the narwhal incident timeline")}); err != nil {
		t.Fatalf("AppendHistory: %v", err)
	}
	if _, err := f.s.SetConversationMultiplayer(f.ctx, "dana@x.com", mp.ID, true, []string{"bob@x.com"}, []string{"carol@x.com"}); err != nil {
		t.Fatalf("SetConversationMultiplayer: %v", err)
	}
	if owner, ok := searchIDs(t, f.s, "bob@x.com", "timeline")[mp.ID]; !ok || owner != "dana@x.com" {
		t.Errorf("participant search: found=%v owner=%q, want dana's chat", ok, owner)
	}
	if got := searchIDs(t, f.s, "carol@x.com", "narwhal"); len(got) != 0 {
		t.Errorf("an excluded person found the multiplayer chat: %v", got)
	}

	// Whatever search returns for a caller is inside their shared list.
	for _, email := range []string{"alice@x.com", "bob@x.com", "carol@x.com", "dana@x.com"} {
		list, err := f.s.ListSharedAndMultiplayerConversations(f.ctx, email)
		if err != nil {
			t.Fatalf("ListSharedAndMultiplayerConversations(%s): %v", email, err)
		}
		allowed := map[string]bool{}
		for _, c := range list {
			allowed[c.ID] = true
		}
		for _, q := range []string{"quokka", "narwhal", "timeline", "spread"} {
			for id, owner := range searchIDs(t, f.s, email, q) {
				if owner != "" && !allowed[id] {
					t.Errorf("%s found %s (owner %s) by %q, which their shared list does not hold", email, id, owner, q)
				}
			}
		}
	}
}
