package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// sharedPinnedIDs returns the IDs the shared listing marks pinned for bob (the
// teammate in these tests), in listing order, and every ID in that listing.
func sharedPinnedIDs(t *testing.T, f teamFixture) (pinned []string, order []string) {
	t.Helper()
	list, err := f.s.ListSharedAndMultiplayerConversations(f.ctx, "bob@x.com")
	if err != nil {
		t.Fatalf("ListSharedAndMultiplayerConversations: %v", err)
	}
	for _, c := range list {
		order = append(order, c.ID)
		if c.Pinned {
			pinned = append(pinned, c.ID)
		}
	}
	return pinned, order
}

// A person's pin on a teammate's chat is theirs alone: it shows in their
// shared listing (pinned first), changes nothing for the owner or for another
// teammate, and the owner's own pin never shows in a teammate's listing.
func TestSharedPin_IsPerViewer(t *testing.T) {
	t.Parallel()

	f := newTeamFixture(t)
	older := f.sharedChat(t, "alice@x.com", f.project.ID, "Older desk")
	newer := f.sharedChat(t, "alice@x.com", f.project.ID, "Newer desk")
	// The owner pins one of her own: that is her arrangement, not bob's.
	if err := f.s.SetPinned(f.ctx, "alice@x.com", newer.ID, true); err != nil {
		t.Fatal(err)
	}
	if pinned, _ := sharedPinnedIDs(t, f); len(pinned) != 0 {
		t.Fatalf("the owner's pin leaked into a teammate's listing: %v", pinned)
	}

	if err := f.s.SetSharedPin(f.ctx, "Bob@X.com", older.ID, true); err != nil {
		t.Fatalf("SetSharedPin: %v", err)
	}
	// Pinning twice is a no-op, not an error.
	if err := f.s.SetSharedPin(f.ctx, "bob@x.com", older.ID, true); err != nil {
		t.Fatalf("SetSharedPin again: %v", err)
	}
	pinned, order := sharedPinnedIDs(t, f)
	if !reflect.DeepEqual(pinned, []string{older.ID}) {
		t.Errorf("bob's pinned = %v, want [%s]", pinned, older.ID)
	}
	if len(order) != 2 || order[0] != older.ID {
		t.Errorf("bob's listing = %v, want his pinned chat first", order)
	}
	if ok, err := f.s.SharedPinned(f.ctx, "bob@x.com", older.ID); err != nil || !ok {
		t.Errorf("SharedPinned(bob) = %v, %v; want true", ok, err)
	}
	if ok, err := f.s.SharedPinned(f.ctx, "bob@x.com", newer.ID); err != nil || ok {
		t.Errorf("SharedPinned(bob, unpinned chat) = %v, %v; want false", ok, err)
	}

	// The owner's row is untouched.
	got, err := f.s.Get(f.ctx, "alice@x.com", older.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Pinned {
		t.Error("a teammate's pin pinned the chat for its owner")
	}

	if err := f.s.SetSharedPin(f.ctx, "bob@x.com", older.ID, false); err != nil {
		t.Fatalf("unpin: %v", err)
	}
	if pinned, _ := sharedPinnedIDs(t, f); len(pinned) != 0 {
		t.Errorf("after unpinning, pinned = %v", pinned)
	}
	// Unpinning what was never pinned is quiet.
	if err := f.s.SetSharedPin(f.ctx, "bob@x.com", older.ID, false); err != nil {
		t.Errorf("unpin again: %v", err)
	}
}

// Only a chat shared with the caller can be pinned: a private chat, another
// team's chat and an unknown ID all answer ErrConversationNotFound and store
// nothing, so a pin can never be used to learn that a chat exists.
func TestSharedPin_OnlyWhatIsSharedWithYou(t *testing.T) {
	t.Parallel()

	f := newTeamFixture(t)
	shared := f.sharedChat(t, "alice@x.com", f.project.ID, "Rates desk")
	private, err := f.s.CreateConversation(f.ctx, "alice@x.com", "Private notes", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ email, id string }{
		"a private chat":          {"bob@x.com", private.ID},
		"another team's chat":     {"dana@x.com", shared.ID},
		"a person with no team":   {"carol@x.com", shared.ID},
		"an unknown chat":         {"bob@x.com", "no-such-chat"},
		"an unknown account":      {"nobody@x.com", shared.ID},
		"no account at all":       {"", shared.ID},
		"the owner's own (owned)": {"alice@x.com", shared.ID},
	} {
		if err := f.s.SetSharedPin(f.ctx, tc.email, tc.id, true); !errors.Is(err, ErrConversationNotFound) {
			t.Errorf("%s: SetSharedPin = %v, want ErrConversationNotFound", name, err)
		}
		if ok, _ := f.s.SharedPinned(f.ctx, tc.email, tc.id); ok {
			t.Errorf("%s: a refused pin was stored", name)
		}
	}

	// Unpinning follows the same rule: a chat that is not shared with the
	// caller, and that they never pinned, is not found.
	for name, tc := range map[string]struct{ email, id string }{
		"a private chat":      {"bob@x.com", private.ID},
		"another team's chat": {"dana@x.com", shared.ID},
		"an unknown chat":     {"bob@x.com", "no-such-chat"},
	} {
		if err := f.s.SetSharedPin(f.ctx, tc.email, tc.id, false); !errors.Is(err, ErrConversationNotFound) {
			t.Errorf("unpin %s: SetSharedPin = %v, want ErrConversationNotFound", name, err)
		}
	}

	// A pin on a chat that stops being shared drops out of the listing with
	// the chat, and comes back with it: the row is a preference, not access.
	if err := f.s.SetSharedPin(f.ctx, "bob@x.com", shared.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetConversationTeamVisible(f.ctx, "alice@x.com", shared.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, order := sharedPinnedIDs(t, f); len(order) != 0 {
		t.Errorf("an unshared chat is still listed for a teammate who pinned it: %v", order)
	}
	// ...and the pin can still be taken off, once: after that the chat is
	// neither pinned nor shared, so it is not found.
	if err := f.s.SetSharedPin(f.ctx, "bob@x.com", shared.ID, false); err != nil {
		t.Errorf("unpinning a chat that stopped being shared = %v, want nil", err)
	}
	if err := f.s.SetSharedPin(f.ctx, "bob@x.com", shared.ID, false); !errors.Is(err, ErrConversationNotFound) {
		t.Errorf("unpinning it again = %v, want ErrConversationNotFound", err)
	}
}

// The pinned order (the rail's hand-set order for the Pinned group) keeps a
// teammate's chat the caller pinned alongside their own pinned chats, and
// still drops one they have not pinned.
func TestChatOrder_PinnedIncludesSharedPins(t *testing.T) {
	t.Parallel()

	f := newTeamFixture(t)
	theirs := f.sharedChat(t, "alice@x.com", f.project.ID, "Rates desk")
	unpinned := f.sharedChat(t, "alice@x.com", f.project.ID, "Not pinned by bob")
	mine, err := f.s.CreateConversation(f.ctx, "bob@x.com", "Mine", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetPinned(f.ctx, "bob@x.com", mine.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetSharedPin(f.ctx, "bob@x.com", theirs.ID, true); err != nil {
		t.Fatal(err)
	}
	kept, err := f.s.SetChatOrder(f.ctx, "bob@x.com", ChatOrderPinned, []string{theirs.ID, unpinned.ID, mine.ID})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{theirs.ID, mine.ID}; !reflect.DeepEqual(kept, want) {
		t.Errorf("kept = %v, want %v", kept, want)
	}
	// Unpinned again, it leaves the next save.
	if err := f.s.SetSharedPin(f.ctx, "bob@x.com", theirs.ID, false); err != nil {
		t.Fatal(err)
	}
	kept, err = f.s.SetChatOrder(f.ctx, "bob@x.com", ChatOrderPinned, []string{theirs.ID, mine.ID})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{mine.ID}; !reflect.DeepEqual(kept, want) {
		t.Errorf("after unpinning, kept = %v, want %v", kept, want)
	}
}

// Deleting an account removes its pins; purging a chat removes every pin on
// it (the foreign key), so neither leaves rows behind.
func TestSharedPin_CleanedUpWithAccountAndChat(t *testing.T) {
	t.Parallel()

	f := newTeamFixture(t)
	a := f.sharedChat(t, "alice@x.com", f.project.ID, "A")
	b := f.sharedChat(t, "alice@x.com", f.project.ID, "B")
	for _, id := range []string{a.ID, b.ID} {
		if err := f.s.SetSharedPin(f.ctx, "bob@x.com", id, true); err != nil {
			t.Fatal(err)
		}
	}
	count := func() int {
		t.Helper()
		var n int
		if err := f.s.db.QueryRowContext(context.Background(),
			`SELECT count(*) FROM chat.shared_conversation_pins`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(); n != 2 {
		t.Fatalf("pins = %d, want 2", n)
	}
	if _, err := f.s.db.ExecContext(f.ctx, `DELETE FROM conversations WHERE id = $1`, a.ID); err != nil {
		t.Fatalf("purge chat: %v", err)
	}
	if n := count(); n != 1 {
		t.Errorf("after purging a chat, pins = %d, want 1", n)
	}
	if err := f.s.DeleteUser(f.ctx, "bob@x.com"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if n := count(); n != 0 {
		t.Errorf("after deleting the account, pins = %d, want 0", n)
	}
}

// Past the cap a new pin is refused with ErrTooManySharedPins; a chat already
// pinned can still be pinned again (a no-op), and unpinning one makes room.
func TestSharedPin_Cap(t *testing.T) {
	t.Parallel()

	f := newTeamFixture(t)
	a := f.sharedChat(t, "alice@x.com", f.project.ID, "A")
	b := f.sharedChat(t, "alice@x.com", f.project.ID, "B")
	const limit = 1
	if err := f.s.setSharedPin(f.ctx, "bob@x.com", a.ID, true, limit); err != nil {
		t.Fatalf("first pin: %v", err)
	}
	if err := f.s.setSharedPin(f.ctx, "bob@x.com", b.ID, true, limit); !errors.Is(err, ErrTooManySharedPins) {
		t.Fatalf("pin past the cap = %v, want ErrTooManySharedPins", err)
	}
	if ok, _ := f.s.SharedPinned(f.ctx, "bob@x.com", b.ID); ok {
		t.Error("a pin past the cap was stored")
	}
	if err := f.s.setSharedPin(f.ctx, "bob@x.com", a.ID, true, limit); err != nil {
		t.Errorf("re-pinning at the cap = %v, want nil", err)
	}
	if err := f.s.setSharedPin(f.ctx, "bob@x.com", a.ID, false, limit); err != nil {
		t.Fatal(err)
	}
	if err := f.s.setSharedPin(f.ctx, "bob@x.com", b.ID, true, limit); err != nil {
		t.Errorf("pin after making room = %v, want nil", err)
	}
}

// Pins racing at the cap cannot both land: with room for one, twelve
// concurrent pins of twelve different chats leave exactly one pinned.
func TestSharedPin_CapHoldsUnderConcurrency(t *testing.T) {
	t.Parallel()

	f := newTeamFixture(t)
	const n = 12
	ids := make([]string, n)
	for i := range ids {
		ids[i] = f.sharedChat(t, "alice@x.com", f.project.ID, "Chat").ID
	}
	errs := make(chan error, n)
	start := make(chan struct{})
	for _, id := range ids {
		go func() {
			<-start
			errs <- f.s.setSharedPin(f.ctx, "bob@x.com", id, true, 1)
		}()
	}
	close(start)
	landed, refused := 0, 0
	for range ids {
		switch err := <-errs; {
		case err == nil:
			landed++
		case errors.Is(err, ErrTooManySharedPins):
			refused++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if landed != 1 || refused != n-1 {
		t.Errorf("landed = %d, refused = %d; want 1 and %d", landed, refused, n-1)
	}
	if pinned, _ := sharedPinnedIDs(t, f); len(pinned) != 1 {
		t.Errorf("pinned = %v, want exactly one", pinned)
	}
}

// A pin row cannot outlive its account: the foreign key removes it with the
// users row even when nothing else does, and refuses a row for no account.
func TestSharedPin_TiedToTheAccount(t *testing.T) {
	t.Parallel()

	f := newTeamFixture(t)
	c := f.sharedChat(t, "alice@x.com", f.project.ID, "A")
	if err := f.s.SetSharedPin(f.ctx, "bob@x.com", c.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.ExecContext(f.ctx,
		`INSERT INTO chat.shared_conversation_pins (user_email, conversation_id, pinned_at) VALUES ('ghost@x.com', $1, 1)`,
		c.ID); !pgForeignKeyViolation(err) {
		t.Errorf("a pin for no account = %v, want a foreign-key violation", err)
	}
	if err := f.s.DeleteUser(f.ctx, "bob@x.com"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	var n int
	if err := f.s.db.QueryRowContext(f.ctx,
		`SELECT count(*) FROM chat.shared_conversation_pins WHERE user_email = 'bob@x.com'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("pins left after the account was deleted = %d", n)
	}
}

// The cap is on the whole Pinned group: the person's own pinned chats count
// toward it, so a pin on a teammate's chat cannot push the group past what
// one saved order holds.
func TestSharedPin_CapCountsOwnPinnedChats(t *testing.T) {
	t.Parallel()

	f := newTeamFixture(t)
	theirs := f.sharedChat(t, "alice@x.com", f.project.ID, "Theirs")
	mine, err := f.s.CreateConversation(f.ctx, "bob@x.com", "Mine", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetPinned(f.ctx, "bob@x.com", mine.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := f.s.setSharedPin(f.ctx, "bob@x.com", theirs.ID, true, 1); !errors.Is(err, ErrTooManySharedPins) {
		t.Fatalf("pin with the group already full of own chats = %v, want ErrTooManySharedPins", err)
	}
	if err := f.s.SetPinned(f.ctx, "bob@x.com", mine.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := f.s.setSharedPin(f.ctx, "bob@x.com", theirs.ID, true, 1); err != nil {
		t.Errorf("pin after unpinning an own chat = %v, want nil", err)
	}
}

// The team listings report the caller's own pin on a teammate's chat, like
// the shared listing: never the owner's, and the caller's own chats keep
// their own.
func TestSharedPin_ShownInTeamListings(t *testing.T) {
	t.Parallel()

	f := newTeamFixture(t)
	ownerPinned := f.sharedChat(t, "alice@x.com", f.project.ID, "Owner pinned")
	viewerPinned := f.sharedChat(t, "alice@x.com", f.project.ID, "Viewer pinned")
	// Shared first, pinned after: filing a chat in the project clears its pin.
	if err := f.s.SetPinned(f.ctx, "alice@x.com", ownerPinned.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetSharedPin(f.ctx, "bob@x.com", viewerPinned.ID, true); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{ownerPinned.ID: false, viewerPinned.ID: true}
	check := func(name string, list []Conversation, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		seen := 0
		for _, c := range list {
			if w, ok := want[c.ID]; ok {
				seen++
				if c.Pinned != w {
					t.Errorf("%s: %q pinned = %v for bob, want %v", name, c.Title, c.Pinned, w)
				}
			}
		}
		if seen != len(want) {
			t.Fatalf("%s listed %d of the %d shared chats", name, seen, len(want))
		}
	}
	list, err := f.s.ListTeamConversations(f.ctx, "bob@x.com")
	check("ListTeamConversations", list, err)
	list, err = f.s.ListProjectTeamConversations(f.ctx, "bob@x.com", f.project.ID)
	check("ListProjectTeamConversations", list, err)

	// The owner reading the same list sees her own pin, not bob's.
	list, err = f.s.ListTeamConversations(f.ctx, "alice@x.com")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range list {
		if c.ID == ownerPinned.ID && !c.Pinned {
			t.Error("the owner's own pin is missing from her team listing")
		}
		if c.ID == viewerPinned.ID && c.Pinned {
			t.Error("a teammate's pin shows on the owner's own chat")
		}
	}
}

// Transferring a chat removes every pin other people had on it, the new
// owner's included, so none is left behind the chat's own pin.
func TestSharedPin_ClearedWhenTheChatIsTransferred(t *testing.T) {
	t.Parallel()

	f := newTeamFixture(t)
	c := f.sharedChat(t, "alice@x.com", f.project.ID, "Handover")
	if err := f.s.SetSharedPin(f.ctx, "bob@x.com", c.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.TransferConversation(f.ctx, "alice@x.com", c.ID, "bob@x.com"); err != nil {
		t.Fatalf("TransferConversation: %v", err)
	}
	var n int
	if err := f.s.db.QueryRowContext(f.ctx,
		`SELECT count(*) FROM chat.shared_conversation_pins WHERE conversation_id = $1`, c.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("pins left on a transferred chat = %d", n)
	}
}
