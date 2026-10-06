package store

import (
	"errors"
	"testing"
	"time"
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

	// Back-date the source's messages so "since" is measurable at 1s grain.
	if _, err := f.s.db.ExecContext(f.ctx, `UPDATE messages SET created_at = created_at - 100 WHERE conversation_id = $1`, c.ID); err != nil {
		t.Fatal(err)
	}
	br, err := f.s.BranchConversation(f.ctx, "bob@x.com", c.ID, last, "Spread study (branch)")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.RecordBranchOrigin(f.ctx, br.ID, BranchOrigin{
		SourceConversationID: c.ID, SourceOwnerEmail: "alice@x.com", SourceTitle: "Spread study",
		BranchedAt:    br.CreatedAt,
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

	// The owner keeps working: messages after the branch flip changed_since.
	if _, err := f.s.db.ExecContext(f.ctx,
		`INSERT INTO messages (conversation_id, role, type, content, created_at) VALUES ($1, 'assistant', 'text', '{"text":"more"}', $2)`,
		c.ID, time.Now().Unix()+5); err != nil {
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
	st, err := f.s.GetProjectUserState(f.ctx, f.project.ID, "bob@x.com")
	if err != nil || st.KeptPersonal || st.HasSharedChat || len(st.SourcesOpen) != 0 {
		t.Fatalf("zero state = %+v, %v", st, err)
	}
	yes := true
	st, err = f.s.UpdateProjectUserState(f.ctx, f.project.ID, "bob@x.com", &yes, map[string]bool{"c1": true})
	if err != nil || !st.KeptPersonal || !st.SourcesOpen["c1"] {
		t.Fatalf("after update = %+v, %v", st, err)
	}
	// sources_open merges; kept_personal untouched when omitted.
	st, err = f.s.UpdateProjectUserState(f.ctx, f.project.ID, "bob@x.com", nil, map[string]bool{"c2": false})
	if err != nil || !st.KeptPersonal || !st.SourcesOpen["c1"] || st.SourcesOpen["c2"] {
		t.Fatalf("after merge = %+v, %v", st, err)
	}
	if _, ok := st.SourcesOpen["c2"]; !ok {
		t.Error("a closed group must be remembered as closed")
	}
	if err := f.s.MarkProjectSharedChat(f.ctx, f.project.ID, "bob@x.com"); err != nil {
		t.Fatal(err)
	}
	st, _ = f.s.GetProjectUserState(f.ctx, f.project.ID, "bob@x.com")
	if !st.HasSharedChat || !st.KeptPersonal {
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
