package store

import (
	"context"
	"errors"
	"testing"
)

// TestTitleLocking pins the #302 contract: the auto-titler (UpdateTitle) may
// retitle a conversation until the user manually renames it (RenameTitle), which
// locks the title; after that UpdateTitle is a no-op returning ErrTitleLocked,
// while a further manual rename still wins.
func TestTitleLocking(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	c, _ := s.CreateConversation(ctx, "u@x.com", "initial", "victoria", "", false)

	// Auto-title works while unlocked.
	if err := s.UpdateTitle(ctx, "u@x.com", c.ID, "auto title"); err != nil {
		t.Fatalf("UpdateTitle (unlocked): %v", err)
	}
	got, _ := s.Get(ctx, "u@x.com", c.ID)
	if got.Title != "auto title" || got.TitleLocked {
		t.Fatalf("after auto-title: %+v", got)
	}

	// Manual rename sets the title AND locks it.
	if err := s.RenameTitle(ctx, "u@x.com", c.ID, "my name"); err != nil {
		t.Fatalf("RenameTitle: %v", err)
	}
	got, _ = s.Get(ctx, "u@x.com", c.ID)
	if got.Title != "my name" || !got.TitleLocked {
		t.Fatalf("after manual rename: %+v", got)
	}

	// Auto-title is now refused and leaves the manual name untouched.
	if err := s.UpdateTitle(ctx, "u@x.com", c.ID, "robot retitle"); !errors.Is(err, ErrTitleLocked) {
		t.Errorf("UpdateTitle on locked title: want ErrTitleLocked, got %v", err)
	}
	if got, _ = s.Get(ctx, "u@x.com", c.ID); got.Title != "my name" {
		t.Errorf("locked title was overwritten: %q", got.Title)
	}

	// A further manual rename still wins (and stays locked).
	if err := s.RenameTitle(ctx, "u@x.com", c.ID, "my name 2"); err != nil {
		t.Fatalf("second RenameTitle: %v", err)
	}
	if got, _ = s.Get(ctx, "u@x.com", c.ID); got.Title != "my name 2" || !got.TitleLocked {
		t.Errorf("after second rename: %+v", got)
	}

	// Cross-user rename is rejected.
	if err := s.RenameTitle(ctx, "intruder@x.com", c.ID, "hax"); err == nil {
		t.Error("foreign RenameTitle should fail")
	}
}
