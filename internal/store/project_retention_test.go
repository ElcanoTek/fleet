package store

import (
	"context"
	"testing"
	"time"
)

// projectChat creates a conversation filed into a fresh project owned by the
// same user, so retention tests exercise the project_id IS NULL filters.
func projectChat(ctx context.Context, t *testing.T, s *Store, email string) *Conversation {
	t.Helper()
	proj, err := s.CreateProject(ctx, &Project{OwnerEmail: email, Name: "Keep"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	c, err := s.CreateConversation(ctx, email, "project chat", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if err := s.SetConversationProject(ctx, email, c.ID, proj.ID); err != nil {
		t.Fatalf("SetConversationProject: %v", err)
	}
	return c
}

// A project conversation must survive the TTL sweep even when long-stale —
// filing into a project is a "keep" state like pin/archive (#509). Red/green:
// before the project_id IS NULL filter, the sweep deleted it.
func TestSweep_ProjectChatExemptFromTTL(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	c := projectChat(ctx, t, s, "u@x.com")
	backdate(ctx, t, s, c.ID, 30*24*time.Hour)

	expired, _, err := s.SweepExpired(ctx, 14*24*time.Hour, 100)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if expired != 0 {
		t.Errorf("project conversation was TTL-swept: %d", expired)
	}
	if got, _ := s.Get(ctx, "u@x.com", c.ID); got == nil {
		t.Error("project conversation must survive the TTL sweep")
	}
}

// Project conversations neither count toward the unpinned cap nor are
// targeted by the cap eviction — mirroring the archived exemption. 4 plain
// active > cap 3 so the eviction loop actually runs.
func TestSweep_ProjectChatExemptFromCap(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	active := make([]string, 4)
	for i := 0; i < 4; i++ {
		c, err := s.CreateConversation(ctx, "u@x.com", "active", "victoria", "", false)
		if err != nil {
			t.Fatalf("create active[%d]: %v", i, err)
		}
		active[i] = c.ID
	}
	base := time.Now().Unix()
	for i, id := range active {
		if _, err := s.db.ExecContext(ctx, `UPDATE conversations SET updated_at = $1 WHERE id = $2`, base-int64(10-i), id); err != nil {
			t.Fatalf("set updated_at: %v", err)
		}
	}
	proj := projectChat(ctx, t, s, "u@x.com")
	// Older than everything, so it would be first out if the filter leaked.
	backdate(ctx, t, s, proj.ID, 60*24*time.Hour)

	_, evicted, err := s.SweepExpired(ctx, 365*24*time.Hour, 3)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if evicted != 1 {
		t.Fatalf("expected exactly 1 plain active evicted (4 active - cap 3), got %d", evicted)
	}
	if got, _ := s.Get(ctx, "u@x.com", active[0]); got != nil {
		t.Error("the oldest plain conversation should have been cap-evicted")
	}
	if got, _ := s.Get(ctx, "u@x.com", proj.ID); got == nil {
		t.Error("project conversation must survive the cap eviction")
	}
}

// Auto-archive must leave project conversations alone: archiving one would
// silently pull it out of its project's rail tree.
func TestAutoArchive_ProjectChatExempt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	c := projectChat(ctx, t, s, "u@x.com")
	backdate(ctx, t, s, c.ID, 60*24*time.Hour)

	if n, err := s.AutoArchiveOlderThan(ctx, 30*24*time.Hour); err != nil || n != 0 {
		t.Fatalf("auto-archive touched project chats: (%d, %v)", n, err)
	}
	if got, _ := s.Get(ctx, "u@x.com", c.ID); got == nil || got.ArchivedAt != nil {
		t.Error("project conversation must stay live and unarchived")
	}
}
