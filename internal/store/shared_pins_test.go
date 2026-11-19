package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// sharedPinnedIDs returns the IDs the shared listing marks pinned for email,
// in listing order, and the position of every ID in that listing.
func sharedPinnedIDs(t *testing.T, f teamFixture, email string) (pinned []string, order []string) {
	t.Helper()
	list, err := f.s.ListSharedAndMultiplayerConversations(f.ctx, email)
	if err != nil {
		t.Fatalf("ListSharedAndMultiplayerConversations(%s): %v", email, err)
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
	if pinned, _ := sharedPinnedIDs(t, f, "bob@x.com"); len(pinned) != 0 {
		t.Fatalf("the owner's pin leaked into a teammate's listing: %v", pinned)
	}

	if err := f.s.SetSharedPin(f.ctx, "Bob@X.com", older.ID, true); err != nil {
		t.Fatalf("SetSharedPin: %v", err)
	}
	// Pinning twice is a no-op, not an error.
	if err := f.s.SetSharedPin(f.ctx, "bob@x.com", older.ID, true); err != nil {
		t.Fatalf("SetSharedPin again: %v", err)
	}
	pinned, order := sharedPinnedIDs(t, f, "bob@x.com")
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
	if pinned, _ := sharedPinnedIDs(t, f, "bob@x.com"); len(pinned) != 0 {
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

	// A pin on a chat that stops being shared drops out of the listing with
	// the chat, and comes back with it: the row is a preference, not access.
	if err := f.s.SetSharedPin(f.ctx, "bob@x.com", shared.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetConversationTeamVisible(f.ctx, "alice@x.com", shared.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, order := sharedPinnedIDs(t, f, "bob@x.com"); len(order) != 0 {
		t.Errorf("an unshared chat is still listed for a teammate who pinned it: %v", order)
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
