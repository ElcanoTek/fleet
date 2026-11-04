package store

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// TestPinOrder pins the rail's saved pinned-chat order: empty until set,
// saved first to last, and limited to the caller's own live pinned chats —
// a foreign, unpinned, deleted, unknown or repeated ID is dropped, never
// stored.
func TestPinOrder(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const me = "trader@elcanotek.com"
	const other = "someone@elcanotek.com"

	got, err := s.PinOrder(ctx, me)
	if err != nil || len(got) != 0 {
		t.Fatalf("fresh order = %v, %v; want empty", got, err)
	}

	mk := func(user, title string, pinned bool) string {
		t.Helper()
		c, err := s.CreateConversation(ctx, user, title, "victoria", "", false)
		if err != nil {
			t.Fatal(err)
		}
		if pinned {
			if err := s.SetPinned(ctx, user, c.ID, true); err != nil {
				t.Fatal(err)
			}
		}
		return c.ID
	}
	a := mk(me, "A", true)
	b := mk(me, "B", true)
	c := mk(me, "C", true)
	loose := mk(me, "unpinned", false)
	theirs := mk(other, "theirs", true)
	gone := mk(me, "deleted", true)
	if err := s.Delete(ctx, me, gone); err != nil {
		t.Fatal(err)
	}

	kept, err := s.SetPinOrder(ctx, me, []string{c, theirs, a, loose, gone, "nope", a, b})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{c, a, b}
	if !reflect.DeepEqual(kept, want) {
		t.Errorf("kept = %v, want %v", kept, want)
	}
	got, err = s.PinOrder(ctx, me)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("read back = %v, %v; want %v", got, err, want)
	}

	// A second save replaces the first.
	if _, err := s.SetPinOrder(ctx, me, []string{b, c}); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.PinOrder(ctx, me); !reflect.DeepEqual(got, []string{b, c}) {
		t.Errorf("after replace = %v", got)
	}
	// Another user's order is their own.
	if got, _ = s.PinOrder(ctx, other); len(got) != 0 {
		t.Errorf("other user's order = %v, want empty", got)
	}

	// Too long is refused.
	long := make([]string, MaxPinOrder+1)
	for i := range long {
		long[i] = a
	}
	if _, err := s.SetPinOrder(ctx, me, long); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Errorf("over-long order err = %v", err)
	}
}
