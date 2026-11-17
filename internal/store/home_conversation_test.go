package store

import (
	"errors"
	"testing"
)

// homeOf reads caller's Home, failing the test on a store error.
func homeOf(t *testing.T, f teamFixture, caller string) *HomeConversation {
	t.Helper()
	h, err := f.s.HomeConversation(f.ctx, caller)
	if err != nil {
		t.Fatalf("HomeConversation(%s): %v", caller, err)
	}
	return h
}

// isPinned reads the owner's pin on convID.
func isPinned(t *testing.T, f teamFixture, owner, convID string) bool {
	t.Helper()
	c, err := f.s.Get(f.ctx, owner, convID)
	if err != nil {
		t.Fatalf("Get(%s): %v", convID, err)
	}
	return c.Pinned
}

// An owned chat becomes Home and is pinned with it; a second choice replaces
// the first (one Home per person); clearing leaves the pin alone.
func TestHomeConversation_OwnChat(t *testing.T) {
	t.Parallel()

	f := newTeamFixture(t)
	if h := homeOf(t, f, "alice@x.com"); h != nil {
		t.Fatalf("fresh Home = %+v, want none", h)
	}
	a, err := f.s.CreateConversation(f.ctx, "alice@x.com", "Morning patrol", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.s.CreateConversation(f.ctx, "alice@x.com", "Other", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}

	got, err := f.s.SetHomeConversation(f.ctx, "alice@x.com", a.ID)
	if err != nil {
		t.Fatalf("SetHomeConversation: %v", err)
	}
	want := HomeConversation{ConversationID: a.ID, Access: HomeAccessOwner, Title: "Morning patrol", OwnerEmail: "alice@x.com"}
	if *got != want {
		t.Errorf("set = %+v, want %+v", *got, want)
	}
	if h := homeOf(t, f, "alice@x.com"); h == nil || *h != want {
		t.Errorf("read back = %+v, want %+v", h, want)
	}
	if !isPinned(t, f, "alice@x.com", a.ID) {
		t.Error("an owned Home must be pinned, or the retention sweep can delete it")
	}

	if _, err := f.s.SetHomeConversation(f.ctx, "alice@x.com", b.ID); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if h := homeOf(t, f, "alice@x.com"); h == nil || h.ConversationID != b.ID {
		t.Errorf("after replace = %+v, want %s", h, b.ID)
	}
	var rows int
	if err := f.s.db.QueryRowContext(f.ctx,
		`SELECT count(*) FROM user_home_conversation WHERE user_email = 'alice@x.com'`).Scan(&rows); err != nil || rows != 1 {
		t.Errorf("home rows = %d (%v), want 1", rows, err)
	}

	if err := f.s.ClearHomeConversation(f.ctx, "alice@x.com"); err != nil {
		t.Fatal(err)
	}
	if h := homeOf(t, f, "alice@x.com"); h != nil {
		t.Errorf("after clear = %+v, want none", h)
	}
	if !isPinned(t, f, "alice@x.com", b.ID) {
		t.Error("clearing Home must not unpin the chat")
	}
	if err := f.s.ClearHomeConversation(f.ctx, "alice@x.com"); err != nil {
		t.Errorf("clearing an absent Home = %v, want nil", err)
	}
}

// A chat the caller cannot open is refused with the same error every read
// gives: unknown, someone else's private chat, deleted, archived.
func TestHomeConversation_RefusesWhatTheCallerCannotOpen(t *testing.T) {
	t.Parallel()

	f := newTeamFixture(t)
	private, err := f.s.CreateConversation(f.ctx, "bob@x.com", "Bob private", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := f.s.CreateConversation(f.ctx, "alice@x.com", "gone", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.Delete(f.ctx, "alice@x.com", deleted.ID); err != nil {
		t.Fatal(err)
	}
	archived, err := f.s.CreateConversation(f.ctx, "alice@x.com", "filed away", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetArchived(f.ctx, "alice@x.com", archived.ID, true); err != nil {
		t.Fatal(err)
	}
	for name, id := range map[string]string{
		"unknown": "no-such-chat", "someone else's": private.ID, "deleted": deleted.ID, "archived": archived.ID,
	} {
		if _, err := f.s.SetHomeConversation(f.ctx, "alice@x.com", id); !errors.Is(err, ErrConversationNotFound) {
			t.Errorf("%s chat: err = %v, want ErrConversationNotFound", name, err)
		}
	}
	if isPinned(t, f, "bob@x.com", private.ID) {
		t.Error("a refused Home must not pin someone else's chat")
	}
	if h := homeOf(t, f, "alice@x.com"); h != nil {
		t.Errorf("Home after refusals = %+v, want none", h)
	}
}

// Unpinning, bulk-unpinning or archiving an owned Home clears it, so an owned
// Home is never left unprotected from retention.
func TestHomeConversation_UnpinClearsOwnHome(t *testing.T) {
	t.Parallel()

	unpin := map[string]func(f teamFixture, id string) error{
		"SetPinned": func(f teamFixture, id string) error {
			return f.s.SetPinned(f.ctx, "alice@x.com", id, false)
		},
		"BulkPatch": func(f teamFixture, id string) error {
			off := false
			_, err := f.s.BulkPatch(f.ctx, "alice@x.com", []string{id}, &off, nil)
			return err
		},
		"SetArchived": func(f teamFixture, id string) error {
			return f.s.SetArchived(f.ctx, "alice@x.com", id, true)
		},
	}
	for name, fn := range unpin {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newTeamFixture(t)
			c, err := f.s.CreateConversation(f.ctx, "alice@x.com", "Home", "victoria", "", false)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.SetHomeConversation(f.ctx, "alice@x.com", c.ID); err != nil {
				t.Fatal(err)
			}
			if err := fn(f, c.ID); err != nil {
				t.Fatal(err)
			}
			if h := homeOf(t, f, "alice@x.com"); h != nil {
				t.Errorf("Home after %s = %+v, want none", name, h)
			}
		})
	}
}

// Pinning (or bulk-pinning) another chat leaves Home alone.
func TestHomeConversation_PinningOtherChatsKeepsHome(t *testing.T) {
	t.Parallel()

	f := newTeamFixture(t)
	home, err := f.s.CreateConversation(f.ctx, "alice@x.com", "Home", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	other, err := f.s.CreateConversation(f.ctx, "alice@x.com", "Other", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetHomeConversation(f.ctx, "alice@x.com", home.ID); err != nil {
		t.Fatal(err)
	}
	on := true
	if _, err := f.s.BulkPatch(f.ctx, "alice@x.com", []string{home.ID, other.ID}, &on, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetPinned(f.ctx, "alice@x.com", other.ID, false); err != nil {
		t.Fatal(err)
	}
	if h := homeOf(t, f, "alice@x.com"); h == nil || h.ConversationID != home.ID {
		t.Errorf("Home = %+v, want %s", h, home.ID)
	}
}

// A teammate's team-shared chat can be Home (read-only team view). Losing the
// share hides it; sharing again brings it back. The owner's pin is neither
// set nor needed, and the owner unpinning their chat leaves the teammate's
// Home alone.
func TestHomeConversation_TeamShared(t *testing.T) {
	t.Parallel()

	f := newTeamFixture(t)
	c := f.sharedChat(t, "bob@x.com", f.project.ID, "Team dashboard")

	got, err := f.s.SetHomeConversation(f.ctx, "alice@x.com", c.ID)
	if err != nil {
		t.Fatalf("teammate SetHomeConversation: %v", err)
	}
	want := HomeConversation{ConversationID: c.ID, Access: HomeAccessTeamView, Title: "Team dashboard", OwnerEmail: "bob@x.com"}
	if *got != want {
		t.Errorf("set = %+v, want %+v", *got, want)
	}
	if isPinned(t, f, "bob@x.com", c.ID) {
		t.Error("a teammate's Home must not pin the owner's chat")
	}
	// Outside the team: refused, like every other read.
	for _, outsider := range []string{"carol@x.com", "dana@x.com"} {
		if _, err := f.s.SetHomeConversation(f.ctx, outsider, c.ID); !errors.Is(err, ErrConversationNotFound) {
			t.Errorf("%s: err = %v, want ErrConversationNotFound", outsider, err)
		}
	}

	if err := f.s.SetPinned(f.ctx, "bob@x.com", c.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetPinned(f.ctx, "bob@x.com", c.ID, false); err != nil {
		t.Fatal(err)
	}
	if h := homeOf(t, f, "alice@x.com"); h == nil || *h != want {
		t.Errorf("after the owner unpinned = %+v, want %+v", h, want)
	}

	if _, err := f.s.SetConversationTeamVisible(f.ctx, "bob@x.com", c.ID, false); err != nil {
		t.Fatal(err)
	}
	if h := homeOf(t, f, "alice@x.com"); h != nil {
		t.Errorf("after unsharing = %+v, want none", h)
	}
	if stored, err := f.s.SetConversationTeamVisible(f.ctx, "bob@x.com", c.ID, true); err != nil || !stored {
		t.Fatalf("re-share = (%v, %v)", stored, err)
	}
	if h := homeOf(t, f, "alice@x.com"); h == nil || *h != want {
		t.Errorf("after re-sharing = %+v, want %+v", h, want)
	}
}

// A Multiplayer chat the caller takes part in opens as a normal chat, and is
// reported that way even when it is also shared with the team.
func TestHomeConversation_Multiplayer(t *testing.T) {
	t.Parallel()

	f := newTeamFixture(t)
	c := f.sharedChat(t, "bob@x.com", f.project.ID, "Live room")
	if _, err := f.s.SetConversationMultiplayer(f.ctx, "bob@x.com", c.ID, true, []string{"alice@x.com"}, nil); err != nil {
		t.Fatalf("SetConversationMultiplayer: %v", err)
	}
	got, err := f.s.SetHomeConversation(f.ctx, "alice@x.com", c.ID)
	if err != nil {
		t.Fatalf("participant SetHomeConversation: %v", err)
	}
	if got.Access != HomeAccessMultiplayer || got.OwnerEmail != "bob@x.com" {
		t.Errorf("set = %+v, want multiplayer access to bob's chat", got)
	}

	// The owner removes alice from the room; the team share still lets her
	// read it, so Home stays and drops to the read-only view.
	if _, err := f.s.UpdateConversationMultiplayerAccess(f.ctx, "bob@x.com", c.ID, nil, []string{"alice@x.com"}); err != nil {
		t.Fatalf("UpdateConversationMultiplayerAccess: %v", err)
	}
	if h := homeOf(t, f, "alice@x.com"); h == nil || h.Access != HomeAccessTeamView {
		t.Errorf("after removal from the room = %+v, want team_view", h)
	}
}

// Both references cascade: a hard-deleted chat takes the row with it, and a
// new account created under a deleted account's address starts with no Home.
func TestHomeConversation_Cascades(t *testing.T) {
	t.Parallel()

	f := newTeamFixture(t)
	c := f.sharedChat(t, "alice@x.com", f.project.ID, "Team dashboard")
	if _, err := f.s.SetHomeConversation(f.ctx, "bob@x.com", c.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DeleteUser(f.ctx, "bob@x.com"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if _, err := f.s.CreateUser(f.ctx, "bob@x.com", "pw-123456"); err != nil {
		t.Fatal(err)
	}
	team := "quant"
	if _, err := f.s.SetUserRoleTeam(f.ctx, "bob@x.com", nil, &team); err != nil {
		t.Fatal(err)
	}
	if h := homeOf(t, f, "bob@x.com"); h != nil {
		t.Errorf("recreated account's Home = %+v, want none", h)
	}

	if _, err := f.s.SetHomeConversation(f.ctx, "bob@x.com", c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.ExecContext(f.ctx, `DELETE FROM conversations WHERE id = $1`, c.ID); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := f.s.db.QueryRowContext(f.ctx,
		`SELECT count(*) FROM user_home_conversation WHERE conversation_id = $1`, c.ID).Scan(&rows); err != nil || rows != 0 {
		t.Errorf("rows after hard delete = %d (%v), want 0", rows, err)
	}
}

// A Home filed in a project is pinned too, and filing keeps that pin, so
// unfiling it later (a move, a revoked share, a deleted project) never leaves
// it to the retention sweep or "Delete temporary chats".
func TestHomeConversation_FiledHomeStaysProtected(t *testing.T) {
	t.Parallel()

	f := newTeamFixture(t)
	c, err := f.s.CreateConversation(f.ctx, "alice@x.com", "Filed", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetConversationProject(f.ctx, "alice@x.com", c.ID, f.project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetHomeConversation(f.ctx, "alice@x.com", c.ID); err != nil {
		t.Fatal(err)
	}
	if !isPinned(t, f, "alice@x.com", c.ID) {
		t.Error("a filed Home must be pinned")
	}
	// Re-filing keeps the Home's pin (filing clears any other chat's).
	other, err := f.s.CreateProject(f.ctx, &Project{OwnerEmail: "alice@x.com", Name: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetConversationProject(f.ctx, "alice@x.com", c.ID, other.ID); err != nil {
		t.Fatal(err)
	}
	if !isPinned(t, f, "alice@x.com", c.ID) {
		t.Error("filing must keep a Home's pin")
	}
	if err := f.s.SetConversationProject(f.ctx, "alice@x.com", c.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DeleteAllUnpinned(f.ctx, "alice@x.com"); err != nil {
		t.Fatal(err)
	}
	if h := homeOf(t, f, "alice@x.com"); h == nil || h.ConversationID != c.ID {
		t.Errorf("after unfiling and Delete temporary chats, Home = %+v, want %s", h, c.ID)
	}

	// A chat that is not Home still loses its pin when filed.
	plain, err := f.s.CreateConversation(f.ctx, "alice@x.com", "Plain", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetPinned(f.ctx, "alice@x.com", plain.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetConversationProject(f.ctx, "alice@x.com", plain.ID, f.project.ID); err != nil {
		t.Fatal(err)
	}
	if isPinned(t, f, "alice@x.com", plain.ID) {
		t.Error("filing must still clear an ordinary chat's pin")
	}
}
