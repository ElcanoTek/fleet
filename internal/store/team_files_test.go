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
	t.Parallel()

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

	// The dialog checklist is one decision over exactly the paths it listed.
	if err := f.s.ApplyOutputChecklist(f.ctx, "alice@x.com", c.ID,
		[]string{"a.csv", "b/c.csv", "d.csv"}, []string{"a.csv", "b/c.csv"}); err != nil {
		t.Fatal(err)
	}
	assertExcluded("after checklist", "a.csv", "b/c.csv", "out/v1.json")
	// All checked: every LISTED exclusion goes. out/v1.json was not in the
	// listing (missing on disk, past the bound…) — the owner made no decision
	// about it, so its exclusion survives and it stays held back if it
	// reappears.
	if err := f.s.ApplyOutputChecklist(f.ctx, "alice@x.com", c.ID,
		[]string{"a.csv", "b/c.csv", "d.csv"}, nil); err != nil {
		t.Fatal(err)
	}
	assertExcluded("after an all-checked checklist", "out/v1.json")
	// An unshared path counts as listed even if the client forgot to list it.
	if err := f.s.ApplyOutputChecklist(f.ctx, "alice@x.com", c.ID, nil, []string{"e.csv"}); err != nil {
		t.Fatal(err)
	}
	assertExcluded("after an unlisted unshare", "e.csv", "out/v1.json")
	if err := f.s.ApplyOutputChecklist(f.ctx, "alice@x.com", c.ID, []string{"e.csv", "out/v1.json"}, nil); err != nil {
		t.Fatal(err)
	}
	assertExcluded("after listing and checking both")
	if err := f.s.ApplyOutputChecklist(f.ctx, "alice@x.com", c.ID, []string{"../x"}, nil); !errors.Is(err, ErrInvalidOutputPath) {
		t.Errorf("invalid listed path err = %v", err)
	}
	if err := f.s.ApplyOutputChecklist(f.ctx, "bob@x.com", c.ID, nil, []string{"x"}); !errors.Is(err, ErrConversationNotFound) {
		t.Errorf("teammate checklist err = %v", err)
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
	t.Parallel()

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
	t.Parallel()

	f := newTeamFixture(t)
	c := f.sharedChat(t, "alice@x.com", f.project.ID, "Spread study")
	view, err := f.s.GetTeamVisibleConversation(f.ctx, "bob@x.com", c.ID)
	if err != nil || view == nil {
		t.Fatalf("team view: %v", err)
	}
	last := view.Messages[len(view.Messages)-1].ID
	// The high-water mark is the branch point the copy used.
	hw := last

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

	// The first-turn note is due (and carries the origin) until the branch
	// commits a user message of its own.
	first, err := f.s.PendingBranchFilesAnnouncement(f.ctx, br.ID)
	if err != nil || first == nil || len(first.CopiedFiles) != 1 {
		t.Fatalf("pending note = %+v, %v", first, err)
	}
	if none, _ := f.s.PendingBranchFilesAnnouncement(f.ctx, c.ID); none != nil {
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
	t.Parallel()

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

// A team link to a Multiplayer chat opens it live for anyone the
// participation rule admits (the rule GET /conversations/{id} applies), and
// the read-only view or a dead end for everyone else — the same answer the
// Team chats row gives, so the two never disagree about one chat.
func TestResolveTeamLinkMultiplayer(t *testing.T) {
	t.Parallel()

	f := newTeamFixture(t)
	c := f.sharedChat(t, "alice@x.com", f.project.ID, "Opti workbench")
	if _, err := f.s.SetConversationMultiplayer(f.ctx, "alice@x.com", c.ID, true, nil, nil); err != nil {
		t.Fatal(err)
	}
	check := func(when, who, wantStatus, wantTeam string) {
		t.Helper()
		tl, err := f.s.ResolveTeamLink(f.ctx, who, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if tl.Status != wantStatus || tl.TeamID != wantTeam || tl.Project != nil {
			t.Errorf("%s: %s: %+v, want %s team=%q", when, who, tl, wantStatus, wantTeam)
		}
	}
	// A teammate the rule admits joins; the owner is still the owner; another
	// team and the teamless are told only the audience, as before.
	check("on", "alice@x.com", TeamLinkOwner, "")
	check("on", "bob@x.com", TeamLinkMultiplayer, "")
	check("on", "dana@x.com", TeamLinkNotOnTeam, "quant")
	check("on", "carol@x.com", TeamLinkNotOnTeam, "quant")

	// Explicitly added from another team: joins, instead of the dead end.
	if _, err := f.s.SetConversationMultiplayer(f.ctx, "alice@x.com", c.ID, true, []string{"dana@x.com"}, nil); err != nil {
		t.Fatal(err)
	}
	check("participant", "dana@x.com", TeamLinkMultiplayer, "")

	// Excluded by the owner: back to the read-only view (the chat is still
	// shared with the team), never the live chat.
	if _, err := f.s.SetConversationMultiplayer(f.ctx, "alice@x.com", c.ID, true, nil, []string{"bob@x.com"}); err != nil {
		t.Fatal(err)
	}
	check("excluded", "bob@x.com", TeamLinkOpen, "")
	if _, err := f.s.SetConversationMultiplayer(f.ctx, "alice@x.com", c.ID, true, nil, []string{}); err != nil {
		t.Fatal(err)
	}
	check("re-included", "bob@x.com", TeamLinkMultiplayer, "")

	// A read-only viewer account cannot take part: read-only view.
	viewer := "viewer"
	if _, err := f.s.SetUserRoleTeam(f.ctx, "bob@x.com", &viewer, nil); err != nil {
		t.Fatal(err)
	}
	check("viewer role", "bob@x.com", TeamLinkOpen, "")
	member := "member"
	if _, err := f.s.SetUserRoleTeam(f.ctx, "bob@x.com", &member, nil); err != nil {
		t.Fatal(err)
	}
	check("member again", "bob@x.com", TeamLinkMultiplayer, "")

	// Lockdown refuses multiplayer: the team share's read-only view stands.
	if _, err := f.s.db.ExecContext(f.ctx, `UPDATE conversations SET lockdown = TRUE WHERE id = $1`, c.ID); err != nil {
		t.Fatal(err)
	}
	check("lockdown", "bob@x.com", TeamLinkOpen, "")
	if _, err := f.s.db.ExecContext(f.ctx, `UPDATE conversations SET lockdown = FALSE WHERE id = $1`, c.ID); err != nil {
		t.Fatal(err)
	}

	// Multiplayer off: the chat stays shared with the team (it lives in the
	// team's project), so the link opens the read-only view again.
	if _, err := f.s.SetConversationMultiplayer(f.ctx, "alice@x.com", c.ID, false, nil, nil); err != nil {
		t.Fatal(err)
	}
	check("off", "bob@x.com", TeamLinkOpen, "")
	check("off", "dana@x.com", TeamLinkNotOnTeam, "quant")

	// Archived: nobody joins and nothing is shared.
	if _, err := f.s.SetConversationMultiplayer(f.ctx, "alice@x.com", c.ID, true, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetArchived(f.ctx, "alice@x.com", c.ID, true); err != nil {
		t.Fatal(err)
	}
	tl, err := f.s.ResolveTeamLink(f.ctx, "bob@x.com", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tl.Status != TeamLinkNotShared {
		t.Errorf("archived: %+v, want not_shared", tl)
	}

	// A teamless owner's Multiplayer chat is open to everyone on the
	// instance: a link to it joins, where it used to say "not shared".
	solo, err := f.s.CreateConversation(f.ctx, "carol@x.com", "Solo", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetConversationMultiplayer(f.ctx, "carol@x.com", solo.ID, true, nil, nil); err != nil {
		t.Fatal(err)
	}
	if tl, err := f.s.ResolveTeamLink(f.ctx, "dana@x.com", solo.ID); err != nil || tl.Status != TeamLinkMultiplayer {
		t.Errorf("teamless owner: %+v, %v, want multiplayer", tl, err)
	}
}

// A branch's origin says whether its owner may take part in the source as a
// Multiplayer chat, read per request: the banner's link back opens the live
// chat while that holds, and the read-only view once it does not.
func TestBranchOriginSourceMultiplayer(t *testing.T) {
	t.Parallel()

	f := newTeamFixture(t)
	c := f.sharedChat(t, "alice@x.com", f.project.ID, "Opti workbench")
	if _, err := f.s.SetConversationMultiplayer(f.ctx, "alice@x.com", c.ID, true, nil, nil); err != nil {
		t.Fatal(err)
	}
	msgs, err := f.s.LoadHistory(f.ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	fork, err := f.s.BranchConversation(f.ctx, "bob@x.com", c.ID, msgs[len(msgs)-1].ID, "fork")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.RecordBranchOrigin(f.ctx, fork.ID, BranchOrigin{
		SourceConversationID: c.ID, SourceOwnerEmail: "alice@x.com", SourceTitle: c.Title, BranchedAt: fork.CreatedAt,
	}); err != nil {
		t.Fatal(err)
	}
	origin := func(when string, wantShared, wantMP bool) {
		t.Helper()
		o, err := f.s.GetBranchOrigin(f.ctx, "bob@x.com", fork.ID)
		if err != nil || o == nil {
			t.Fatalf("%s: origin = %+v, %v", when, o, err)
		}
		if o.SourceStillShared != wantShared || o.SourceMultiplayer != wantMP {
			t.Errorf("%s: still_shared=%v multiplayer=%v, want %v/%v", when, o.SourceStillShared, o.SourceMultiplayer, wantShared, wantMP)
		}
	}
	origin("multiplayer", true, true)
	if _, err := f.s.SetConversationMultiplayer(f.ctx, "alice@x.com", c.ID, true, nil, []string{"bob@x.com"}); err != nil {
		t.Fatal(err)
	}
	origin("excluded", true, false)
	if _, err := f.s.SetConversationMultiplayer(f.ctx, "alice@x.com", c.ID, false, nil, []string{}); err != nil {
		t.Fatal(err)
	}
	origin("multiplayer off", true, false)
}

func TestProjectUserState(t *testing.T) {
	t.Parallel()

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
	if err := f.s.MarkProjectSharedChat(f.ctx, c2.ID, "bob@x.com"); err != nil {
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
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

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
	// A write that lands after the deletion (a request admitted before it)
	// cannot leave state behind for a future account under the same address.
	if _, err := f.s.UpdateProjectUserState(f.ctx, f.project.ID, "bob@x.com", &yes, nil); err == nil {
		t.Error("a state write for a deleted account succeeded")
	}
	// (The shared-chat mark reads the chat, which went with the account: it
	// has nothing to record, and the count below confirms no row came back.)
	_ = f.s.MarkProjectSharedChat(f.ctx, "gone", "bob@x.com")
	if err := f.s.db.QueryRowContext(f.ctx,
		`SELECT COUNT(*) FROM project_user_state WHERE user_email = 'bob@x.com'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d rows (%v) for the deleted account after late writes", n, err)
	}
}

// An archived chat cannot be team-shared, and unarchive never reopens team
// access — even for a row that somehow carries team_visible while archived
// (a share stored before the archived refusal existed). Without both, "share
// while archived, then unarchive" re-exposed the chat with no share action.
func TestArchivedChatCannotBeSharedAndUnarchiveStaysPrivate(t *testing.T) {
	t.Parallel()

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

// The high-water mark is the branch point, passed in — never re-read from
// the source when the origin is recorded. A source message that lands between
// the branch's copy and the origin write is not in the branch, so it must
// report changed_since.
func TestBranchOriginHighWaterIsNotReadAfterCopy(t *testing.T) {
	t.Parallel()

	f := newTeamFixture(t)
	c := f.sharedChat(t, "alice@x.com", f.project.ID, "Spread study")
	var hw int64
	if err := f.s.db.QueryRowContext(f.ctx,
		`SELECT COALESCE(MAX(id), 0) FROM messages WHERE conversation_id = $1`, c.ID).Scan(&hw); err != nil || hw == 0 {
		t.Fatalf("max message id = %d, %v", hw, err)
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

// changed_since counts only what the team view shows. The branch point is the
// last visible text id; a completed turn persists a turn_summary (and other
// non-text rows) after it, which must not make a fresh branch read "changed".
func TestViewerBranchesIgnoresInvisibleRows(t *testing.T) {
	t.Parallel()

	f := newTeamFixture(t)
	c := f.sharedChat(t, "alice@x.com", f.project.ID, "Spread study")
	view, err := f.s.GetTeamVisibleConversation(f.ctx, "bob@x.com", c.ID)
	if err != nil || view == nil || len(view.Messages) == 0 {
		t.Fatalf("team view: %v", err)
	}
	last := view.Messages[len(view.Messages)-1].ID
	for _, row := range [][2]string{
		{"assistant", "turn_summary"}, {"assistant", "tool_call"}, {"tool", "tool_result"}, {"assistant", "reasoning"},
	} {
		if _, err := f.s.db.ExecContext(f.ctx,
			`INSERT INTO messages (conversation_id, role, type, content, created_at) VALUES ($1, $2, $3, '{}', 0)`,
			c.ID, row[0], row[1]); err != nil {
			t.Fatal(err)
		}
	}
	br, err := f.s.BranchConversation(f.ctx, "bob@x.com", c.ID, last, "b")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.RecordBranchOrigin(f.ctx, br.ID, BranchOrigin{
		SourceConversationID: c.ID, SourceOwnerEmail: "alice@x.com", SourceTitle: "Spread study",
		BranchedAt: br.CreatedAt, SourceMaxMessageID: last,
	}); err != nil {
		t.Fatal(err)
	}
	vb, err := f.s.ViewerBranches(f.ctx, "bob@x.com", []string{c.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := vb[c.ID]; !ok || got.ChangedSince {
		t.Errorf("viewer branch = %+v (ok=%v): rows the team view hides must not report changed_since", got, ok)
	}
	if _, err := f.s.db.ExecContext(f.ctx,
		`INSERT INTO messages (conversation_id, role, type, content, created_at) VALUES ($1, 'user', 'text', '{"text":"next"}', 0)`,
		c.ID); err != nil {
		t.Fatal(err)
	}
	vb, _ = f.s.ViewerBranches(f.ctx, "bob@x.com", []string{c.ID})
	if !vb[c.ID].ChangedSince {
		t.Error("a later visible text row must report changed_since")
	}
}

// The first-turn note is derived from committed rows, not a latch: it stays
// due across any number of turns that fail (or a process that dies) before a
// user message commits — nothing to release, nothing a crash can strand — and
// is never due again once one has. The copied transcript's own user rows
// (ids below the branch's recorded high-water mark) do not count. The
// truncation flag round-trips with the origin.
func TestBranchFilesNoteDueUntilAUserTurnCommits(t *testing.T) {
	t.Parallel()

	f := newTeamFixture(t)
	c := f.sharedChat(t, "alice@x.com", f.project.ID, "Spread study")
	view, err := f.s.GetTeamVisibleConversation(f.ctx, "bob@x.com", c.ID)
	if err != nil || view == nil {
		t.Fatalf("team view: %v", err)
	}
	last := view.Messages[len(view.Messages)-1].ID
	br, err := f.s.BranchConversation(f.ctx, "bob@x.com", c.ID, last, "branch")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.RecordBranchOrigin(f.ctx, br.ID, BranchOrigin{
		SourceConversationID: c.ID, SourceOwnerEmail: "alice@x.com", BranchedAt: br.CreatedAt,
		SourceMaxMessageID: last, WithheldTruncated: true,
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ { // turns that never committed: still due, every time
		o, err := f.s.PendingBranchFilesAnnouncement(f.ctx, br.ID)
		if err != nil || o == nil || !o.WithheldTruncated {
			t.Fatalf("read %d: pending = %+v, %v", i, o, err)
		}
	}
	// An assistant row alone (no user turn) does not settle it either.
	if _, err := f.s.db.ExecContext(f.ctx,
		`INSERT INTO messages (conversation_id, role, type, content, created_at) VALUES ($1, 'assistant', 'text', '{"text":"x"}', 0)`,
		br.ID); err != nil {
		t.Fatal(err)
	}
	if o, _ := f.s.PendingBranchFilesAnnouncement(f.ctx, br.ID); o == nil {
		t.Error("no user turn has committed yet; the note is still due")
	}
	// The first user turn commits (carrying the note): never due again.
	if _, err := f.s.db.ExecContext(f.ctx,
		`INSERT INTO messages (conversation_id, role, type, content, created_at) VALUES ($1, 'user', 'text', '{"text":"go"}', 0)`,
		br.ID); err != nil {
		t.Fatal(err)
	}
	if o, _ := f.s.PendingBranchFilesAnnouncement(f.ctx, br.ID); o != nil {
		t.Error("the note is due again after a user turn committed")
	}
}

// "The viewer's most recent branch" survives branches made in the same
// second: branched_at is whole seconds and branch ids are random, so the
// recording order (seq) breaks the tie — for ViewerBranches and for the
// team-view version that feeds the conditional poll alike.
func TestViewerBranchesSameSecondUsesRecordingOrder(t *testing.T) {
	t.Parallel()

	f := newTeamFixture(t)
	c := f.sharedChat(t, "alice@x.com", f.project.ID, "Spread study")
	view, err := f.s.GetTeamVisibleConversation(f.ctx, "bob@x.com", c.ID)
	if err != nil || view == nil || len(view.Messages) == 0 {
		t.Fatalf("team view: %v", err)
	}
	last := view.Messages[len(view.Messages)-1].ID
	versions := map[string]bool{}
	for i := 0; i < 6; i++ {
		br, err := f.s.BranchConversation(f.ctx, "bob@x.com", c.ID, last, fmt.Sprintf("b%d", i))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.s.RecordBranchOrigin(f.ctx, br.ID, BranchOrigin{
			SourceConversationID: c.ID, SourceOwnerEmail: "alice@x.com", SourceTitle: "Spread study",
			BranchedAt: 1767225600, SourceMaxMessageID: last, // one shared second
		}); err != nil {
			t.Fatal(err)
		}
		vb, err := f.s.ViewerBranches(f.ctx, "bob@x.com", []string{c.ID})
		if err != nil {
			t.Fatal(err)
		}
		if got := vb[c.ID].ConversationID; got != br.ID {
			t.Fatalf("branch %d: most recent = %s, want the one just made (%s)", i, got, br.ID)
		}
		v, err := f.s.TeamViewVersion(f.ctx, "bob@x.com", c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if versions[v] {
			t.Fatalf("branch %d: team-view version %q repeats an earlier one", i, v)
		}
		versions[v] = true
	}
}

// The exclusion fingerprint is unambiguous: two different sets whose paths
// join to the same newline-separated string still version differently.
func TestTeamViewVersionExclusionSetsDoNotCollide(t *testing.T) {
	t.Parallel()

	f := newTeamFixture(t)
	c := f.sharedChat(t, "alice@x.com", f.project.ID, "Spread study")
	version := func(paths ...string) string {
		t.Helper()
		if _, err := f.s.db.ExecContext(f.ctx,
			`DELETE FROM conversation_output_exclusions WHERE conversation_id = $1`, c.ID); err != nil {
			t.Fatal(err)
		}
		for _, p := range paths {
			if _, err := f.s.db.ExecContext(f.ctx,
				`INSERT INTO conversation_output_exclusions (conversation_id, path, created_at) VALUES ($1, $2, 0)`, c.ID, p); err != nil {
				t.Fatal(err)
			}
		}
		v, err := f.s.TeamViewVersion(f.ctx, "bob@x.com", c.ID)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if a, b := version("a\nb", "c"), version("a", "b\nc"); a == b {
		t.Errorf("exclusion sets {a\\nb, c} and {a, b\\nc} share version %q", a)
	}
}

// The exclusion set is bounded per chat: a write that would take it past
// maxExclusionsPerConversation is refused whole (nothing inserted), and
// re-sharing — a delete — still works at the cap. Nothing is pruned.
func TestExclusionsCappedPerConversation(t *testing.T) {
	t.Parallel()

	f := newTeamFixture(t)
	c := f.sharedChat(t, "alice@x.com", f.project.ID, "Spread study")
	if _, err := f.s.db.ExecContext(f.ctx, `
		INSERT INTO conversation_output_exclusions (conversation_id, path, created_at)
		SELECT $1, 'old/' || g, 0 FROM generate_series(1, $2::int) g`, c.ID, maxExclusionsPerConversation); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		var n int
		if err := f.s.db.QueryRowContext(f.ctx,
			`SELECT COUNT(*) FROM conversation_output_exclusions WHERE conversation_id = $1`, c.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := f.s.SetOutputShared(f.ctx, "alice@x.com", c.ID, "new.csv", false); !errors.Is(err, ErrTooManyExclusions) {
		t.Fatalf("SetOutputShared past the cap = %v, want ErrTooManyExclusions", err)
	}
	if err := f.s.ApplyOutputChecklist(f.ctx, "alice@x.com", c.ID, []string{"x.csv", "y.csv"}, []string{"x.csv"}); !errors.Is(err, ErrTooManyExclusions) {
		t.Fatalf("ApplyOutputChecklist past the cap = %v, want ErrTooManyExclusions", err)
	}
	if n := count(); n != maxExclusionsPerConversation {
		t.Fatalf("a refused write left %d exclusions, want %d", n, maxExclusionsPerConversation)
	}
	// Already-excluded paths are not new rows: re-asserting one is fine.
	if err := f.s.SetOutputShared(f.ctx, "alice@x.com", c.ID, "old/1", false); err != nil {
		t.Fatalf("re-excluding an existing path at the cap: %v", err)
	}
	if err := f.s.SetOutputShared(f.ctx, "alice@x.com", c.ID, "old/1", true); err != nil {
		t.Fatalf("re-sharing at the cap: %v", err)
	}
	if err := f.s.SetOutputShared(f.ctx, "alice@x.com", c.ID, "new.csv", false); err != nil {
		t.Fatalf("one under the cap: %v", err)
	}
}

// Sources state keeps only groups the caller could see: their own chats and
// chats shared with their team. A key naming another member's PRIVATE chat
// in the project is dropped exactly like a deleted one, so the returned map
// never tells a member that private chat still exists here.
func TestSourcesOpenKeepsOnlyVisibleChats(t *testing.T) {
	t.Parallel()

	f := newTeamFixture(t)
	shared := f.sharedChat(t, "alice@x.com", f.project.ID, "Shared")
	private, err := f.s.CreateConversation(f.ctx, "alice@x.com", "Private", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetConversationProject(f.ctx, "alice@x.com", private.ID, f.project.ID); err != nil {
		t.Fatal(err)
	}
	mine, err := f.s.CreateConversation(f.ctx, "bob@x.com", "Mine", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetConversationProject(f.ctx, "bob@x.com", mine.ID, f.project.ID); err != nil {
		t.Fatal(err)
	}
	st, err := f.s.UpdateProjectUserState(f.ctx, f.project.ID, "bob@x.com", nil,
		map[string]bool{shared.ID: true, private.ID: true, mine.ID: false, "gone": true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.SourcesOpen[private.ID]; ok {
		t.Error("a teammate's private chat kept its Sources key")
	}
	if _, ok := st.SourcesOpen["gone"]; ok {
		t.Error("a missing chat kept its Sources key")
	}
	if _, ok := st.SourcesOpen[shared.ID]; !ok {
		t.Error("a chat shared with the team lost its key")
	}
	if _, ok := st.SourcesOpen[mine.ID]; !ok {
		t.Error("the caller's own chat lost its key")
	}
}

// The shared-chat mark credits the project the chat is filed in when the mark
// is written — the one that actually hosts the share — not a project read
// before a racing refile. A chat that is no longer shared credits nothing.
func TestMarkProjectSharedChatCreditsTheChatsCurrentProject(t *testing.T) {
	t.Parallel()

	f := newTeamFixture(t)
	c := f.sharedChat(t, "bob@x.com", f.project.ID, "Moving")
	second, err := f.s.CreateProject(f.ctx, &Project{OwnerEmail: "alice@x.com", Name: "Second", TeamID: "quant"})
	if err != nil {
		t.Fatal(err)
	}
	// A refile to another quant project raced the share: the share stands
	// (same team), and the mark must land on the project it moved to.
	if err := f.s.SetConversationProject(f.ctx, "bob@x.com", c.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.MarkProjectSharedChat(f.ctx, c.ID, "bob@x.com"); err != nil {
		t.Fatal(err)
	}
	if st, _ := f.s.GetProjectUserState(f.ctx, second.ID, "bob@x.com"); !st.HasSharedChat {
		t.Error("the project hosting the share was not credited")
	}
	if st, _ := f.s.GetProjectUserState(f.ctx, f.project.ID, "bob@x.com"); st.HasSharedChat {
		t.Error("the project the chat left was credited")
	}
	// Unshared in between: nothing is credited.
	if _, err := f.s.SetConversationTeamVisible(f.ctx, "bob@x.com", c.ID, false); err != nil {
		t.Fatal(err)
	}
	other := f.sharedChat(t, "alice@x.com", f.project.ID, "Alice's")
	if _, err := f.s.SetConversationTeamVisible(f.ctx, "alice@x.com", other.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := f.s.MarkProjectSharedChat(f.ctx, other.ID, "alice@x.com"); err != nil {
		t.Fatal(err)
	}
	if st, _ := f.s.GetProjectUserState(f.ctx, f.project.ID, "alice@x.com"); st.HasSharedChat {
		t.Error("a chat no longer shared credited its project")
	}
}
