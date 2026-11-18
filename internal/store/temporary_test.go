package store

import (
	"testing"
	"time"
)

// "Delete temporary chats" removes exactly the chats the retention sweep
// would: a chat that something keeps — a pin, a project, a public link, team
// visibility, Multiplayer, an armed patrol or timer — survives it. It used to
// take every unpinned, unfiled chat, so a chat the rail did not mark as
// temporary could be deleted by the button that names them.
func TestDeleteAllUnpinned_KeepsWhatTheSweepKeeps(t *testing.T) {
	t.Parallel()

	for _, soft := range []bool{false, true} {
		name := "hard delete"
		if soft {
			name = "soft delete"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newTeamFixture(t)
			f.s.SetSoftDelete(soft)
			const me = "alice@x.com"
			mk := func(title string) *Conversation {
				t.Helper()
				c, err := f.s.CreateConversation(f.ctx, me, title, "victoria", "", false)
				if err != nil {
					t.Fatalf("CreateConversation(%s): %v", title, err)
				}
				return c
			}

			temporary := mk("temporary")
			pinned := mk("pinned")
			if err := f.s.SetPinned(f.ctx, me, pinned.ID, true); err != nil {
				t.Fatalf("SetPinned: %v", err)
			}
			linked := mk("shared by link")
			if err := f.s.SetShareToken(f.ctx, me, linked.ID, "tok-"+name, nil); err != nil {
				t.Fatalf("SetShareToken: %v", err)
			}
			team := f.sharedChat(t, me, f.project.ID, "shared with the team")
			multiplayer := mk("multiplayer")
			if _, err := f.s.SetConversationMultiplayer(f.ctx, me, multiplayer.ID, true, nil, nil); err != nil {
				t.Fatalf("SetConversationMultiplayer: %v", err)
			}
			patrol := mk("has a patrol")
			if _, err := f.s.CreateChatSchedule(f.ctx, ChatScheduleInput{
				ConversationID: patrol.ID, UserEmail: me, Prompt: "check in", CronExpr: "0 9 * * *",
			}); err != nil {
				t.Fatalf("CreateChatSchedule(patrol): %v", err)
			}
			timer := mk("has a timer")
			if _, err := f.s.CreateChatSchedule(f.ctx, ChatScheduleInput{
				ConversationID: timer.ID, UserEmail: me, Prompt: "remind me", DurationSeconds: 3600,
			}); err != nil {
				t.Fatalf("CreateChatSchedule(timer): %v", err)
			}
			// A cancelled patrol keeps nothing.
			ended := mk("patrol ended")
			sc, err := f.s.CreateChatSchedule(f.ctx, ChatScheduleInput{
				ConversationID: ended.ID, UserEmail: me, Prompt: "check in", CronExpr: "0 9 * * *",
			})
			if err != nil {
				t.Fatalf("CreateChatSchedule(ended): %v", err)
			}
			if _, err := f.s.CancelChatSchedule(f.ctx, me, ended.ID, sc.ID); err != nil {
				t.Fatalf("CancelChatSchedule: %v", err)
			}

			// The listing says which chats a schedule keeps.
			list, err := f.s.ListFiltered(f.ctx, me, ListFilter{})
			if err != nil {
				t.Fatalf("ListFiltered: %v", err)
			}
			scheduled := map[string]bool{}
			for _, c := range list {
				if c.HasSchedule {
					scheduled[c.ID] = true
				}
			}
			if len(scheduled) != 2 || !scheduled[patrol.ID] || !scheduled[timer.ID] {
				t.Errorf("HasSchedule set on %v, want exactly the patrol and timer chats", scheduled)
			}

			n, err := f.s.DeleteAllUnpinned(f.ctx, me)
			if err != nil {
				t.Fatalf("DeleteAllUnpinned: %v", err)
			}
			if n != 2 {
				t.Errorf("deleted %d, want 2 (the temporary chat and the one whose patrol ended)", n)
			}
			for _, gone := range []*Conversation{temporary, ended} {
				if got, _ := f.s.Get(f.ctx, me, gone.ID); got != nil {
					t.Errorf("%q should have been deleted", gone.Title)
				}
			}
			for _, kept := range []*Conversation{pinned, linked, team, multiplayer, patrol, timer} {
				if got, _ := f.s.Get(f.ctx, me, kept.ID); got == nil {
					t.Errorf("%q must survive Delete temporary chats", kept.Title)
				}
			}

			// And the sweep agrees: aged far past the TTL, the same chats stay.
			old := time.Now().Add(-400 * 24 * time.Hour).Unix()
			if _, err := f.s.db.Exec(`UPDATE conversations SET updated_at = $1 WHERE user_email = $2`, old, me); err != nil {
				t.Fatal(err)
			}
			if _, _, err := f.s.SweepExpired(f.ctx, 24*time.Hour, 0); err != nil {
				t.Fatalf("SweepExpired: %v", err)
			}
			for _, kept := range []*Conversation{pinned, linked, team, multiplayer, patrol, timer} {
				if got, _ := f.s.Get(f.ctx, me, kept.ID); got == nil {
					t.Errorf("%q must survive the retention sweep", kept.Title)
				}
			}
		})
	}
}
