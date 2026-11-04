package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestChatOrder_Pinned pins the rail's saved pinned-chat order: empty until
// set, saved first to last, and limited to the caller's own live pinned chats
// — a foreign, unpinned, deleted, unknown or repeated ID is dropped, never
// stored.
func TestChatOrder_Pinned(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const me = "trader@elcanotek.com"
	const other = "someone@elcanotek.com"

	got, err := s.ChatOrders(ctx, me)
	if err != nil || len(got) != 0 {
		t.Fatalf("fresh orders = %v, %v; want none", got, err)
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

	kept, err := s.SetChatOrder(ctx, me, ChatOrderPinned, []string{c, theirs, a, loose, gone, "nope", a, b})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{c, a, b}
	if !reflect.DeepEqual(kept, want) {
		t.Errorf("kept = %v, want %v", kept, want)
	}
	got, err = s.ChatOrders(ctx, me)
	if err != nil || !reflect.DeepEqual(got[ChatOrderPinned], want) {
		t.Errorf("read back = %v, %v; want %v", got, err, want)
	}

	// A second save replaces the first.
	if _, err := s.SetChatOrder(ctx, me, ChatOrderPinned, []string{b, c}); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.ChatOrders(ctx, me); !reflect.DeepEqual(got[ChatOrderPinned], []string{b, c}) {
		t.Errorf("after replace = %v", got)
	}
	// Another user's orders are their own.
	if got, _ = s.ChatOrders(ctx, other); len(got) != 0 {
		t.Errorf("other user's orders = %v, want none", got)
	}
	// A list that keeps nothing removes the row.
	if _, err := s.SetChatOrder(ctx, me, ChatOrderPinned, []string{theirs}); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.ChatOrders(ctx, me); len(got) != 0 {
		t.Errorf("after an empty save = %v, want none", got)
	}

	// Too long, and a bad scope, are refused.
	long := make([]string, MaxChatOrder+1)
	for i := range long {
		long[i] = a
	}
	if _, err := s.SetChatOrder(ctx, me, ChatOrderPinned, long); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Errorf("over-long order err = %v", err)
	}
	for _, bad := range []string{"", "temporary", "project:", "pinned "} {
		if _, err := s.SetChatOrder(ctx, me, bad, []string{a}); !errors.Is(err, ErrBadChatOrderScope) {
			t.Errorf("scope %q err = %v, want ErrBadChatOrderScope", bad, err)
		}
	}
}

// TestChatOrder_Project pins a project's chat order: only the caller's own
// chats filed in THAT project are kept, and deleting the project removes the
// order.
func TestChatOrder_Project(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const me = "builder@elcanotek.com"
	p, err := s.CreateProject(ctx, &Project{OwnerEmail: me, Name: "Roadmap"})
	if err != nil {
		t.Fatal(err)
	}
	q, err := s.CreateProject(ctx, &Project{OwnerEmail: me, Name: "Elsewhere"})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(projectID string) string {
		t.Helper()
		c, err := s.CreateConversation(ctx, me, "chat", "victoria", "", false)
		if err != nil {
			t.Fatal(err)
		}
		if projectID != "" {
			if err := s.SetConversationProject(ctx, me, c.ID, projectID); err != nil {
				t.Fatal(err)
			}
		}
		return c.ID
	}
	x, y := mk(p.ID), mk(p.ID)
	inOther := mk(q.ID)
	unfiled := mk("")
	scope := ChatOrderProjectScope(p.ID)
	kept, err := s.SetChatOrder(ctx, me, scope, []string{y, inOther, unfiled, x})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(kept, []string{y, x}) {
		t.Errorf("kept = %v, want [y x]", kept)
	}
	if err := s.DeleteProject(ctx, me, p.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ChatOrders(ctx, me); len(got) != 0 {
		t.Errorf("orders after DeleteProject = %v, want none", got)
	}
}

// TestChatOrder_GoesWithTheAccount pins that deleting an account purges its
// saved orders, so an address re-provisioned later never reads the old IDs.
func TestChatOrder_GoesWithTheAccount(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const me = "leaver@elcanotek.com"
	if _, err := s.CreateUser(ctx, me, "correct-horse-battery"); err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateConversation(ctx, me, "A", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPinned(ctx, me, c.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetChatOrder(ctx, me, ChatOrderPinned, []string{c.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser(ctx, me); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM conversation_order WHERE user_email = $1`, me).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("order rows after DeleteUser = %d, want 0", n)
	}
}
