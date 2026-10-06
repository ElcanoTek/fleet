package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// Exclusions are the owner's per-file choices: owner-only, validated, and
// independent of whether the chat is shared — they survive stop sharing,
// sharing again, archive and unarchive (ADR-0079).
func TestOutputExclusionsSurviveSharingChanges(t *testing.T) {
	f := newTeamFixture(t)
	c := f.sharedChat(t, "alice@x.com", f.project.ID, "Q3")

	if err := f.s.SetOutputShared(f.ctx, "alice@x.com", c.ID, "out/v1.json", false); err != nil {
		t.Fatalf("exclude: %v", err)
	}
	// A teammate cannot change the owner's choices.
	if err := f.s.SetOutputShared(f.ctx, "bob@x.com", c.ID, "out/v1.json", true); !errors.Is(err, ErrConversationNotFound) {
		t.Errorf("teammate toggle err = %v, want ErrConversationNotFound", err)
	}
	for _, bad := range []string{"", "../x", "a//b", "/abs", "a/./b", `a\b`} {
		if err := f.s.SetOutputShared(f.ctx, "alice@x.com", c.ID, bad, false); !errors.Is(err, ErrInvalidOutputPath) {
			t.Errorf("path %q err = %v, want ErrInvalidOutputPath", bad, err)
		}
	}

	assertExcluded := func(when string, want ...string) {
		t.Helper()
		got, err := f.s.ListOutputExclusions(f.ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("%s: exclusions = %v, want %v", when, got, want)
		}
		for _, p := range want {
			if !got[p] {
				t.Fatalf("%s: exclusions = %v, want %v", when, got, want)
			}
		}
	}
	assertExcluded("after uncheck", "out/v1.json")

	if _, err := f.s.SetConversationTeamVisible(f.ctx, "alice@x.com", c.ID, false); err != nil {
		t.Fatal(err)
	}
	assertExcluded("after stop sharing", "out/v1.json")
	if _, err := f.s.SetConversationTeamVisible(f.ctx, "alice@x.com", c.ID, true); err != nil {
		t.Fatal(err)
	}
	assertExcluded("after sharing again", "out/v1.json")

	// Archive unshares (team_visible + audience cleared); unarchive comes back
	// Only you; the file choices are untouched throughout.
	if err := f.s.SetArchived(f.ctx, "alice@x.com", c.ID, true); err != nil {
		t.Fatal(err)
	}
	if pid, shared := filingState(t, f, c.ID); shared || pid != f.project.ID {
		t.Errorf("after archive: project=%q shared=%v, want still filed and unshared", pid, shared)
	}
	if err := f.s.SetArchived(f.ctx, "alice@x.com", c.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, shared := filingState(t, f, c.ID); shared {
		t.Error("unarchive must come back as Only you")
	}
	if ok, _ := f.s.CanTeamRead(f.ctx, "bob@x.com", c.ID); ok {
		t.Error("an unarchived chat must not be readable by the team until shared again")
	}
	assertExcluded("after archive/unarchive", "out/v1.json")

	// The dialog checklist replaces the set as one decision.
	if err := f.s.ReplaceOutputExclusions(f.ctx, "alice@x.com", c.ID, []string{"a.csv", "b/c.csv"}); err != nil {
		t.Fatal(err)
	}
	assertExcluded("after replace", "a.csv", "b/c.csv")
	if err := f.s.ReplaceOutputExclusions(f.ctx, "alice@x.com", c.ID, nil); err != nil {
		t.Fatal(err)
	}
	assertExcluded("after replace with none")
	if err := f.s.ReplaceOutputExclusions(f.ctx, "bob@x.com", c.ID, []string{"x"}); !errors.Is(err, ErrConversationNotFound) {
		t.Errorf("teammate replace err = %v", err)
	}

	// Rows die with the chat.
	if err := f.s.SetOutputShared(f.ctx, "alice@x.com", c.ID, "z.csv", false); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Delete(f.ctx, "alice@x.com", c.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.s.ListOutputExclusions(f.ctx, c.ID); len(got) != 0 {
		t.Errorf("exclusions outlived their chat: %v", got)
	}
}

// Moving a shared chat between two projects shared with the SAME team keeps
// it shared (and its file choices with it); moving it anywhere else unshares.
func TestMoveBetweenSameTeamProjectsKeepsSharing(t *testing.T) {
	f := newTeamFixture(t)
	c := f.sharedChat(t, "alice@x.com", f.project.ID, "Q3")
	other, err := f.s.CreateProject(f.ctx, &Project{OwnerEmail: "bob@x.com", Name: "Quant 2", TeamID: "quant"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetOutputShared(f.ctx, "alice@x.com", c.ID, "held.csv", false); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetConversationProject(f.ctx, "alice@x.com", c.ID, other.ID); err != nil {
		t.Fatal(err)
	}
	if pid, shared := filingState(t, f, c.ID); pid != other.ID || !shared {
		t.Errorf("same-team move: project=%q shared=%v, want %q/true", pid, shared, other.ID)
	}
	personal, err := f.s.CreateProject(f.ctx, &Project{OwnerEmail: "alice@x.com", Name: "Mine"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetConversationProject(f.ctx, "alice@x.com", c.ID, personal.ID); err != nil {
		t.Fatal(err)
	}
	if _, shared := filingState(t, f, c.ID); shared {
		t.Error("a move into a personal project must unshare")
	}
	if got, _ := f.s.ListOutputExclusions(f.ctx, c.ID); !got["held.csv"] {
		t.Error("a move must not touch the owner's file choices")
	}
}

// A teammate branch records where it came from; the viewer listing finds the
// most recent live branch and says whether the source gained messages since.
func TestBranchOriginAndViewerBranches(t *testing.T) {
	f := newTeamFixture(t)
	c := f.sharedChat(t, "alice@x.com", f.project.ID, "Spread study")
	view, err := f.s.GetTeamVisibleConversation(f.ctx, "bob@x.com", c.ID)
	if err != nil || view == nil {
		t.Fatalf("team view: %v", err)
	}
	last := view.Messages[len(view.Messages)-1].ID
	hw, err := f.s.MaxMessageID(f.ctx, c.ID)
	if err != nil || hw < last {
		t.Fatalf("MaxMessageID = %d, %v (last visible %d)", hw, err, last)
	}

	br, err := f.s.BranchConversation(f.ctx, "bob@x.com", c.ID, last, "Spread study (branch)")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.RecordBranchOrigin(f.ctx, br.ID, BranchOrigin{
		SourceConversationID: c.ID, SourceOwnerEmail: "alice@x.com", SourceTitle: "Spread study",
		BranchedAt: br.CreatedAt, SourceMaxMessageID: hw,
		CopiedFiles:   []BranchFile{{Path: "out/a.csv", Name: "a.csv", Size: 3}},
		WithheldFiles: []string{"out/held.csv"},
	}); err != nil {
		t.Fatal(err)
	}

	o, err := f.s.GetBranchOrigin(f.ctx, "bob@x.com", br.ID)
	if err != nil || o == nil {
		t.Fatalf("GetBranchOrigin = %v, %v", o, err)
	}
	if o.SourceOwnerEmail != "alice@x.com" || o.SourceTitle != "Spread study" || !o.SourceStillShared ||
		len(o.CopiedFiles) != 1 || o.CopiedFiles[0].Path != "out/a.csv" || len(o.WithheldFiles) != 1 {
		t.Errorf("origin = %+v", o)
	}
	// Only the branch's owner reads its origin; the source has none.
	if o, _ := f.s.GetBranchOrigin(f.ctx, "alice@x.com", br.ID); o != nil {
		t.Error("someone else's branch origin must not resolve")
	}
	if o, _ := f.s.GetBranchOrigin(f.ctx, "alice@x.com", c.ID); o != nil {
		t.Error("a chat that is not a teammate branch has no origin")
	}

	vb, err := f.s.ViewerBranches(f.ctx, "bob@x.com", []string{c.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := vb[c.ID]; !ok || got.ConversationID != br.ID || got.ChangedSince {
		t.Errorf("viewer branch = %+v (ok=%v), want %s, unchanged", got, ok, br.ID)
	}
	if other, _ := f.s.ViewerBranches(f.ctx, "dana@x.com", []string{c.ID}); len(other) != 0 {
		t.Error("another user has no branch of it")
	}

	// The owner keeps working: messages after the branch flip changed_since —
	// even one written in the SAME second as the branch (created_at and
	// branched_at are whole seconds; the id high-water mark is not).
	if _, err := f.s.db.ExecContext(f.ctx,
		`INSERT INTO messages (conversation_id, role, type, content, created_at) VALUES ($1, 'assistant', 'text', '{"text":"more"}', $2)`,
		c.ID, br.CreatedAt); err != nil {
		t.Fatal(err)
	}
	vb, _ = f.s.ViewerBranches(f.ctx, "bob@x.com", []string{c.ID})
	if !vb[c.ID].ChangedSince {
		t.Error("new messages in the source must report changed_since")
	}

	// The first-turn note is claimed exactly once.
	first, err := f.s.ClaimBranchFilesAnnouncement(f.ctx, br.ID)
	if err != nil || first == nil || len(first.CopiedFiles) != 1 {
		t.Fatalf("first claim = %+v, %v", first, err)
	}
	if again, _ := f.s.ClaimBranchFilesAnnouncement(f.ctx, br.ID); again != nil {
		t.Error("the note must be claimed once")
	}
	if none, _ := f.s.ClaimBranchFilesAnnouncement(f.ctx, c.ID); none != nil {
		t.Error("a non-branch has nothing to announce")
	}

	// Unsharing the source: the origin says so (the banner stops linking
	// back), and the branch is untouched.
	if _, err := f.s.SetConversationTeamVisible(f.ctx, "alice@x.com", c.ID, false); err != nil {
		t.Fatal(err)
	}
	if o, _ := f.s.GetBranchOrigin(f.ctx, "bob@x.com", br.ID); o == nil || o.SourceStillShared {
		t.Errorf("after unshare origin = %+v, want source_still_shared=false", o)
	}
	// Deleting the branch drops it from the viewer listing.
	if err := f.s.Delete(f.ctx, "bob@x.com", br.ID); err != nil {
		t.Fatal(err)
	}
	if vb, _ := f.s.ViewerBranches(f.ctx, "bob@x.com", []string{c.ID}); len(vb) != 0 {
		t.Errorf("a deleted branch must not be reported: %+v", vb)
	}
}

// The team link reveals only what routing needs.
func TestResolveTeamLink(t *testing.T) {
	f := newTeamFixture(t)
	c := f.sharedChat(t, "alice@x.com", f.project.ID, "Spread study")

	check := func(who, id, wantStatus, wantTeam, wantProject string) {
		t.Helper()
		tl, err := f.s.ResolveTeamLink(f.ctx, who, id)
		if err != nil {
			t.Fatal(err)
		}
		gotProject := ""
		if tl.Project != nil {
			gotProject = tl.Project.ID
		}
		if tl.Status != wantStatus || tl.TeamID != wantTeam || gotProject != wantProject {
			t.Errorf("%s/%s: %+v, want %s team=%q project=%q", who, id, tl, wantStatus, wantTeam, wantProject)
		}
	}
	check("alice@x.com", c.ID, TeamLinkOwner, "", "")
	check("bob@x.com", c.ID, TeamLinkOpen, "", "")
	check("dana@x.com", c.ID, TeamLinkNotOnTeam, "quant", "")
	check("carol@x.com", c.ID, TeamLinkNotOnTeam, "quant", "")
	check("bob@x.com", "no-such-id", TeamLinkNotShared, "", "")

	// Unshared: a teammate who can still see the project is offered it; an
	// outsider is told nothing about it.
	if _, err := f.s.SetConversationTeamVisible(f.ctx, "alice@x.com", c.ID, false); err != nil {
		t.Fatal(err)
	}
	check("bob@x.com", c.ID, TeamLinkNotShared, "", f.project.ID)
	check("dana@x.com", c.ID, TeamLinkNotShared, "", "")

	// Archived reads as not shared too.
	if _, err := f.s.SetConversationTeamVisible(f.ctx, "alice@x.com", c.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetArchived(f.ctx, "alice@x.com", c.ID, true); err != nil {
		t.Fatal(err)
	}
	check("bob@x.com", c.ID, TeamLinkNotShared, "", f.project.ID)
	check("dana@x.com", c.ID, TeamLinkNotShared, "", "")
}

func TestProjectUserState(t *testing.T) {
	f := newTeamFixture(t)
	c1 := f.sharedChat(t, "alice@x.com", f.project.ID, "One")
	c2 := f.sharedChat(t, "bob@x.com", f.project.ID, "Two")
	st, err := f.s.GetProjectUserState(f.ctx, f.project.ID, "bob@x.com")
	if err != nil || st.KeptPersonal || st.HasSharedChat || len(st.SourcesOpen) != 0 {
		t.Fatalf("zero state = %+v, %v", st, err)
	}
	yes := true
	st, err = f.s.UpdateProjectUserState(f.ctx, f.project.ID, "bob@x.com", &yes, map[string]bool{c1.ID: true})
	if err != nil || !st.KeptPersonal || !st.SourcesOpen[c1.ID] {
		t.Fatalf("after update = %+v, %v", st, err)
	}
	// sources_open merges; kept_personal untouched when omitted (nil never
	// overwrites a stored value).
	st, err = f.s.UpdateProjectUserState(f.ctx, f.project.ID, "bob@x.com", nil, map[string]bool{c2.ID: false})
	if err != nil || !st.KeptPersonal || !st.SourcesOpen[c1.ID] || st.SourcesOpen[c2.ID] {
		t.Fatalf("after merge = %+v, %v", st, err)
	}
	if _, ok := st.SourcesOpen[c2.ID]; !ok {
		t.Error("a closed group must be remembered as closed")
	}
	if err := f.s.MarkProjectSharedChat(f.ctx, f.project.ID, "bob@x.com"); err != nil {
		t.Fatal(err)
	}
	st, _ = f.s.GetProjectUserState(f.ctx, f.project.ID, "bob@x.com")
	if !st.HasSharedChat || !st.KeptPersonal || !st.SourcesOpen[c1.ID] {
		t.Errorf("after mark = %+v", st)
	}
	// Per person: alice's state is her own.
	if other, _ := f.s.GetProjectUserState(f.ctx, f.project.ID, "alice@x.com"); other.KeptPersonal || other.HasSharedChat {
		t.Errorf("state leaked across people: %+v", other)
	}
	// Dies with the project.
	if err := f.s.DeleteProject(f.ctx, "alice@x.com", f.project.ID); err != nil {
		t.Fatal(err)
	}
	if st, _ := f.s.GetProjectUserState(f.ctx, f.project.ID, "bob@x.com"); st.HasSharedChat {
		t.Error("project state outlived its project")
	}
}

// Concurrent partial updates from two devices both land: the merge runs under
// the row lock, so a kept_personal write and a sources_open write never
// overwrite each other with a stale read.
func TestProjectUserStateConcurrentPartialUpdates(t *testing.T) {
	f := newTeamFixture(t)
	const n = 8
	chats := make([]*Conversation, n)
	for i := range chats {
		chats[i] = f.sharedChat(t, "bob@x.com", f.project.ID, fmt.Sprintf("c%d", i))
	}
	yes := true
	var wg sync.WaitGroup
	errs := make(chan error, n+1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := f.s.UpdateProjectUserState(f.ctx, f.project.ID, "bob@x.com", &yes, nil)
		errs <- err
	}()
	for _, c := range chats {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_, err := f.s.UpdateProjectUserState(f.ctx, f.project.ID, "bob@x.com", nil, map[string]bool{id: true})
			errs <- err
		}(c.ID)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	st, err := f.s.GetProjectUserState(f.ctx, f.project.ID, "bob@x.com")
	if err != nil {
		t.Fatal(err)
	}
	if !st.KeptPersonal {
		t.Error("kept_personal lost to a concurrent sources_open write")
	}
	for _, c := range chats {
		if !st.SourcesOpen[c.ID] {
			t.Errorf("open state for %s lost to a concurrent write: %v", c.Title, st.SourcesOpen)
		}
	}
}

// A full sources_open map never refuses a new choice: keys for chats that left
// the project or were deleted are pruned, and if it is still over the cap the
// untouched stored keys are evicted — the key being written always lands.
func TestProjectUserStateSourcesOpenCap(t *testing.T) {
	f := newTeamFixture(t)
	gone := f.sharedChat(t, "bob@x.com", f.project.ID, "gone")
	kept := f.sharedChat(t, "bob@x.com", f.project.ID, "kept")
	fresh := f.sharedChat(t, "bob@x.com", f.project.ID, "fresh")
	if _, err := f.s.UpdateProjectUserState(f.ctx, f.project.ID, "bob@x.com", nil,
		map[string]bool{gone.ID: true, kept.ID: true}); err != nil {
		t.Fatal(err)
	}
	// A dead chat's key is pruned on the next write.
	if err := f.s.Delete(f.ctx, "bob@x.com", gone.ID); err != nil {
		t.Fatal(err)
	}
	st, err := f.s.UpdateProjectUserState(f.ctx, f.project.ID, "bob@x.com", nil, map[string]bool{fresh.ID: false})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.SourcesOpen[gone.ID]; ok {
		t.Errorf("deleted chat's key survived: %v", st.SourcesOpen)
	}
	if !st.SourcesOpen[kept.ID] {
		t.Errorf("live chat's key pruned: %v", st.SourcesOpen)
	}

	// Fill the map to the cap with live chats (seeded directly: creating 500
	// chats through the API is slow and beside the point), then write a new
	// key: it lands, and the map stays at the cap.
	ids := make([]string, 0, maxSourcesOpenEntries)
	full := map[string]bool{}
	for i := 0; i < maxSourcesOpenEntries; i++ {
		id := fmt.Sprintf("filler-%04d", i)
		ids = append(ids, id)
		full[id] = true
	}
	if _, err := f.s.db.ExecContext(f.ctx, `
		INSERT INTO conversations (id, user_email, title, project_id, created_at, updated_at)
		SELECT x, 'bob@x.com', 'filler', $2, 0, 0 FROM unnest($1::text[]) AS x`,
		ids, f.project.ID); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(full)
	if _, err := f.s.db.ExecContext(f.ctx,
		`UPDATE project_user_state SET sources_open = $3::jsonb WHERE project_id = $1 AND user_email = $2`,
		f.project.ID, "bob@x.com", string(raw)); err != nil {
		t.Fatal(err)
	}
	st, err = f.s.UpdateProjectUserState(f.ctx, f.project.ID, "bob@x.com", nil, map[string]bool{fresh.ID: true})
	if err != nil {
		t.Fatal(err)
	}
	if !st.SourcesOpen[fresh.ID] {
		t.Error("a new choice on a full map must land")
	}
	if len(st.SourcesOpen) != maxSourcesOpenEntries {
		t.Errorf("len = %d, want the cap %d", len(st.SourcesOpen), maxSourcesOpenEntries)
	}
	if got, _ := f.s.GetProjectUserState(f.ctx, f.project.ID, "bob@x.com"); !got.SourcesOpen[fresh.ID] {
		t.Error("the new choice must be stored, not just echoed")
	}
}

// Deleting a user removes their per-project UI state — including in projects
// they did not own, which the project cascade never reaches.
func TestDeleteUserRemovesProjectUserState(t *testing.T) {
	f := newTeamFixture(t)
	yes := true
	if _, err := f.s.UpdateProjectUserState(f.ctx, f.project.ID, "bob@x.com", &yes, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.UpdateProjectUserState(f.ctx, f.project.ID, "alice@x.com", &yes, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DeleteUser(f.ctx, "bob@x.com"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.s.db.QueryRowContext(f.ctx,
		`SELECT COUNT(*) FROM project_user_state WHERE user_email = 'bob@x.com'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d project_user_state rows outlived their user", n)
	}
	if st, _ := f.s.GetProjectUserState(f.ctx, f.project.ID, "alice@x.com"); !st.KeptPersonal {
		t.Error("another user's state must be untouched")
	}
}

// An archived chat cannot be team-shared, and unarchive never reopens team
// access — even for a row that somehow carries team_visible while archived
// (a share stored before the archived refusal existed). Without both, "share
// while archived, then unarchive" re-exposed the chat with no share action.
func TestArchivedChatCannotBeSharedAndUnarchiveStaysPrivate(t *testing.T) {
	f := newTeamFixture(t)
	c := f.sharedChat(t, "alice@x.com", f.project.ID, "Spread study")
	if err := f.s.SetArchived(f.ctx, "alice@x.com", c.ID, true); err != nil {
		t.Fatal(err)
	}
	stored, err := f.s.SetConversationTeamVisible(f.ctx, "alice@x.com", c.ID, true)
	if !errors.Is(err, ErrArchivedNotShareable) || stored {
		t.Fatalf("share while archived = (%v, %v), want (false, ErrArchivedNotShareable)", stored, err)
	}
	if _, shared := filingState(t, f, c.ID); shared {
		t.Fatal("a refused share must store nothing")
	}
	// Stopping sharing is never refused, archived or not.
	if _, err := f.s.SetConversationTeamVisible(f.ctx, "alice@x.com", c.ID, false); err != nil {
		t.Fatalf("unshare while archived: %v", err)
	}

	// A legacy row: archived AND team_visible. Unarchive must clear it.
	if _, err := f.s.db.ExecContext(f.ctx,
		`UPDATE conversations SET team_visible = TRUE, team_shared_with = $2 WHERE id = $1`,
		c.ID, f.project.TeamID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetArchived(f.ctx, "alice@x.com", c.ID, false); err != nil {
		t.Fatal(err)
	}
	var visible bool
	var with *string
	if err := f.s.db.QueryRowContext(f.ctx,
		`SELECT team_visible, team_shared_with FROM conversations WHERE id = $1`, c.ID).Scan(&visible, &with); err != nil {
		t.Fatal(err)
	}
	if visible || with != nil {
		t.Errorf("after unarchive team_visible=%v team_shared_with=%v, want false/NULL", visible, with)
	}
	if ok, _ := f.s.CanTeamRead(f.ctx, "bob@x.com", c.ID); ok {
		t.Error("unarchive must not reopen team access")
	}
	// Unarchived, it can be shared again.
	if stored, err := f.s.SetConversationTeamVisible(f.ctx, "alice@x.com", c.ID, true); err != nil || !stored {
		t.Errorf("share after unarchive = (%v, %v)", stored, err)
	}
}

// The high-water mark is the one read BEFORE the branch, passed in — never
// re-read when the origin is recorded. A source message that lands between
// the branch's copy and the origin write is not in the branch, so it must
// report changed_since.
func TestBranchOriginHighWaterIsNotReadAfterCopy(t *testing.T) {
	f := newTeamFixture(t)
	c := f.sharedChat(t, "alice@x.com", f.project.ID, "Spread study")
	hw, err := f.s.MaxMessageID(f.ctx, c.ID)
	if err != nil || hw == 0 {
		t.Fatalf("MaxMessageID = %d, %v", hw, err)
	}
	br, err := f.s.BranchConversation(f.ctx, "bob@x.com", c.ID, hw, "b")
	if err != nil {
		t.Fatal(err)
	}
	// Mid-branch: the owner's next message arrives after the copy.
	if _, err := f.s.db.ExecContext(f.ctx,
		`INSERT INTO messages (conversation_id, role, type, content, created_at) VALUES ($1, 'assistant', 'text', '{"text":"late"}', $2)`,
		c.ID, br.CreatedAt); err != nil {
		t.Fatal(err)
	}
	if err := f.s.RecordBranchOrigin(f.ctx, br.ID, BranchOrigin{
		SourceConversationID: c.ID, SourceOwnerEmail: "alice@x.com", SourceTitle: "Spread study",
		BranchedAt: br.CreatedAt, SourceMaxMessageID: hw,
	}); err != nil {
		t.Fatal(err)
	}
	vb, err := f.s.ViewerBranches(f.ctx, "bob@x.com", []string{c.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := vb[c.ID]; !ok || !got.ChangedSince {
		t.Errorf("viewer branch = %+v (ok=%v): a message after the snapshot must report changed_since", got, ok)
	}
}
