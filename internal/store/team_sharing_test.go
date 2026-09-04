package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// teamFixture provisions alice + bob on one team, dana on another, and a
// team-shared project owned by alice — the smallest world in which "a chat
// shared with the team, inside a project shared with the team" is expressible.
type teamFixture struct {
	s       *Store
	ctx     context.Context
	project *Project
}

func newTeamFixture(t *testing.T) teamFixture {
	t.Helper()
	s := newTestStore(t)
	ctx := context.Background()
	for _, u := range []struct{ email, team string }{
		{"alice@x.com", "quant"}, {"bob@x.com", "quant"},
		{"dana@x.com", "ops"}, {"carol@x.com", ""},
	} {
		if _, err := s.CreateUser(ctx, u.email, "pw-123456"); err != nil {
			t.Fatalf("CreateUser %s: %v", u.email, err)
		}
		team := u.team
		if _, err := s.SetUserRoleTeam(ctx, u.email, nil, &team); err != nil {
			t.Fatalf("SetUserRoleTeam %s: %v", u.email, err)
		}
	}
	p, err := s.CreateProject(ctx, &Project{
		OwnerEmail: "alice@x.com", Name: "Quant", TeamID: "quant",
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	return teamFixture{s: s, ctx: ctx, project: p}
}

// sharedChat creates a conversation owned by email, files it into projectID,
// marks it team-visible, and gives it one user + one assistant message so the
// transcript and the branch point are real.
func (f teamFixture) sharedChat(t *testing.T, email, projectID, title string) *Conversation {
	t.Helper()
	c, err := f.s.CreateConversation(f.ctx, email, title, "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if projectID != "" {
		if err := f.s.SetConversationProject(f.ctx, email, c.ID, projectID); err != nil {
			t.Fatalf("SetConversationProject: %v", err)
		}
	}
	if _, err := f.s.AppendHistory(f.ctx, c.ID, []agent.HistoryEntry{
		{Role: "user", Type: "text", Content: []byte(`{"text":"what is the spread?"}`)},
		{Role: "assistant", Type: "tool_call", Content: []byte(`{"tool":"run_bash","args":"cat /etc/passwd"}`)},
		{Role: "assistant", Type: "text", Content: []byte(`{"text":"about 12 bps"}`)},
	}); err != nil {
		t.Fatalf("AppendHistory: %v", err)
	}
	if stored, err := f.s.SetConversationTeamVisible(f.ctx, email, c.ID, true); err != nil || !stored {
		t.Fatalf("SetConversationTeamVisible = (%v, %v), want (true, nil)", stored, err)
	}
	return c
}

// A teammate can read a chat its owner shared with the team; nobody else can,
// and the refusal is indistinguishable from "no such chat".
func TestGetTeamVisibleConversation_Gates(t *testing.T) {
	f := newTeamFixture(t)
	c := f.sharedChat(t, "alice@x.com", f.project.ID, "Spread study")

	got, err := f.s.GetTeamVisibleConversation(f.ctx, "bob@x.com", c.ID)
	if err != nil {
		t.Fatalf("teammate read: %v", err)
	}
	if got == nil {
		t.Fatal("a teammate must be able to read a team-shared chat")
	}
	if got.OwnerEmail != "alice@x.com" || got.ProjectID != f.project.ID {
		t.Errorf("snapshot owner/project = %q/%q", got.OwnerEmail, got.ProjectID)
	}
	// Transcript only: the tool_call entry must never reach a teammate — its
	// content is the agent's working trace, not what the owner shared.
	if len(got.Messages) != 2 {
		t.Fatalf("want 2 text messages, got %d: %+v", len(got.Messages), got.Messages)
	}
	for _, m := range got.Messages {
		if m.Type != "text" {
			t.Errorf("non-text entry leaked into the team view: %+v", m)
		}
		if m.ID == 0 {
			t.Error("message ids must survive: Branch forks at one")
		}
	}

	// Another team, and no team at all: nothing.
	for _, who := range []string{"dana@x.com", "carol@x.com"} {
		out, err := f.s.GetTeamVisibleConversation(f.ctx, who, c.ID)
		if err != nil {
			t.Fatalf("%s read: %v", who, err)
		}
		if out != nil {
			t.Errorf("%s must not read another team's chat", who)
		}
	}

	// Un-sharing closes the door again, for the teammate too.
	if _, err := f.s.SetConversationTeamVisible(f.ctx, "alice@x.com", c.ID, false); err != nil {
		t.Fatal(err)
	}
	if out, _ := f.s.GetTeamVisibleConversation(f.ctx, "bob@x.com", c.ID); out != nil {
		t.Error("an unshared chat must stop being readable by the team")
	}
}

// The ADR-0057 pairing: a team-shared chat always has a place to appear, so
// every way of taking that place away also unshares the chat.
func TestTeamShareRequiresATeamSharedProject(t *testing.T) {
	t.Run("moving out of the project unshares", func(t *testing.T) {
		f := newTeamFixture(t)
		personal, err := f.s.CreateProject(f.ctx, &Project{OwnerEmail: "alice@x.com", Name: "Mine"})
		if err != nil {
			t.Fatal(err)
		}
		c := f.sharedChat(t, "alice@x.com", f.project.ID, "Study")

		if err := f.s.SetConversationProject(f.ctx, "alice@x.com", c.ID, personal.ID); err != nil {
			t.Fatal(err)
		}
		if got, _ := f.s.Get(f.ctx, "alice@x.com", c.ID); got.TeamVisible {
			t.Error("moving into a PERSONAL project must unshare the chat")
		}
	})

	t.Run("unfiling unshares", func(t *testing.T) {
		f := newTeamFixture(t)
		c := f.sharedChat(t, "alice@x.com", f.project.ID, "Study")
		if err := f.s.SetConversationProject(f.ctx, "alice@x.com", c.ID, ""); err != nil {
			t.Fatal(err)
		}
		if got, _ := f.s.Get(f.ctx, "alice@x.com", c.ID); got.TeamVisible {
			t.Error("removing a chat from its project must unshare it")
		}
	})

	t.Run("moving between team-shared projects keeps the share", func(t *testing.T) {
		f := newTeamFixture(t)
		other, err := f.s.CreateProject(f.ctx, &Project{
			OwnerEmail: "alice@x.com", Name: "Quant II", TeamID: "quant",
		})
		if err != nil {
			t.Fatal(err)
		}
		c := f.sharedChat(t, "alice@x.com", f.project.ID, "Study")
		if err := f.s.SetConversationProject(f.ctx, "alice@x.com", c.ID, other.ID); err != nil {
			t.Fatal(err)
		}
		if got, _ := f.s.Get(f.ctx, "alice@x.com", c.ID); !got.TeamVisible {
			t.Error("the chat still has a team-shared home; the share must survive")
		}
	})

	t.Run("un-sharing the project unshares its chats", func(t *testing.T) {
		f := newTeamFixture(t)
		c := f.sharedChat(t, "alice@x.com", f.project.ID, "Study")
		none := ""
		if _, err := f.s.UpdateProject(f.ctx, "alice@x.com", f.project.ID, ProjectPatch{TeamID: &none}); err != nil {
			t.Fatal(err)
		}
		if got, _ := f.s.Get(f.ctx, "alice@x.com", c.ID); got.TeamVisible {
			t.Error("a project that stopped being team-shared must not leave shared chats behind")
		}
	})

	t.Run("deleting the project unshares and detaches", func(t *testing.T) {
		f := newTeamFixture(t)
		c := f.sharedChat(t, "alice@x.com", f.project.ID, "Study")
		if err := f.s.DeleteProject(f.ctx, "alice@x.com", f.project.ID); err != nil {
			t.Fatal(err)
		}
		got, _ := f.s.Get(f.ctx, "alice@x.com", c.ID)
		if got == nil {
			t.Fatal("deleting a project must keep its members' chats")
		}
		if got.TeamVisible || got.ProjectID != "" {
			t.Errorf("after delete: team_visible=%v project=%q", got.TeamVisible, got.ProjectID)
		}
	})

	t.Run("leaving the team unshares the leaver's chats", func(t *testing.T) {
		f := newTeamFixture(t)
		// Bob shares one of his own chats into alice's team project.
		bobChat := f.sharedChat(t, "bob@x.com", f.project.ID, "Bob's study")
		aliceChat := f.sharedChat(t, "alice@x.com", f.project.ID, "Alice's study")

		if _, err := f.s.SetOwnTeam(f.ctx, "bob@x.com", "", false); err != nil {
			t.Fatalf("leave: %v", err)
		}
		if got, _ := f.s.Get(f.ctx, "bob@x.com", bobChat.ID); got.TeamVisible {
			t.Error("leaving a team must unshare the chats you shared with it")
		}
		// Everyone else's shares are untouched — leaving is not a group action.
		if got, _ := f.s.Get(f.ctx, "alice@x.com", aliceChat.ID); !got.TeamVisible {
			t.Error("one member leaving must not unshare another member's chat")
		}
	})
}

// The project home's Team section lists teammates' shared chats in THIS
// project — not the caller's own, not other projects', not unshared ones.
func TestListProjectTeamConversations(t *testing.T) {
	f := newTeamFixture(t)
	other, err := f.s.CreateProject(f.ctx, &Project{
		OwnerEmail: "alice@x.com", Name: "Elsewhere", TeamID: "quant",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := f.sharedChat(t, "alice@x.com", f.project.ID, "Alice shared")
	f.sharedChat(t, "bob@x.com", f.project.ID, "Bob's own")   // caller's own
	f.sharedChat(t, "alice@x.com", other.ID, "Wrong project") // another project
	// A private chat of alice's in the same project.
	priv, err := f.s.CreateConversation(f.ctx, "alice@x.com", "Private", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetConversationProject(f.ctx, "alice@x.com", priv.ID, f.project.ID); err != nil {
		t.Fatal(err)
	}

	list, err := f.s.ListProjectTeamConversations(f.ctx, "bob@x.com", f.project.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != want.ID {
		t.Fatalf("want only alice's shared chat in this project, got %d: %+v", len(list), list)
	}
	if list[0].ShareToken != "" {
		t.Error("a teammate listing must never carry the owner's public share token")
	}

	// Another team sees nothing; a teamless caller gets an empty list, not an
	// error (the section simply renders its empty state).
	if l, err := f.s.ListProjectTeamConversations(f.ctx, "dana@x.com", f.project.ID); err != nil || len(l) != 0 {
		t.Errorf("other team: %d rows, err=%v", len(l), err)
	}
	if l, err := f.s.ListProjectTeamConversations(f.ctx, "carol@x.com", f.project.ID); err != nil || len(l) != 0 {
		t.Errorf("teamless: %d rows, err=%v", len(l), err)
	}
}

// A teammate can branch a chat shared with the team, and the fork lands in the
// same project, owned by the brancher. The original is untouched, and survives
// the original being unshared afterwards.
func TestBranchFromTeamSharedConversation(t *testing.T) {
	f := newTeamFixture(t)
	c := f.sharedChat(t, "alice@x.com", f.project.ID, "Spread study")
	snap, err := f.s.GetTeamVisibleConversation(f.ctx, "bob@x.com", c.ID)
	if err != nil || snap == nil {
		t.Fatalf("team view: %v", err)
	}
	point := snap.Messages[len(snap.Messages)-1].ID

	branch, err := f.s.BranchConversation(f.ctx, "bob@x.com", c.ID, point, "Bob's fork")
	if err != nil {
		t.Fatalf("branch: %v", err)
	}
	if branch.UserEmail != "bob@x.com" {
		t.Errorf("branch owner = %q, want the brancher", branch.UserEmail)
	}
	if branch.ProjectID != f.project.ID {
		t.Errorf("branch project = %q, want the parent's project", branch.ProjectID)
	}
	if branch.TeamVisible {
		t.Error("a branch must start private — sharing is its owner's choice")
	}

	// Un-sharing the original leaves the branch alone: it is a copy.
	if _, err := f.s.SetConversationTeamVisible(f.ctx, "alice@x.com", c.ID, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.s.Get(f.ctx, "bob@x.com", branch.ID); got == nil {
		t.Fatal("a branch must survive the original being unshared")
	}
	msgs, err := f.s.LoadHistory(f.ctx, branch.ID)
	if err != nil || len(msgs) == 0 {
		t.Fatalf("branch history: %d entries, err=%v", len(msgs), err)
	}

	// A chat nobody shared with him is still not branchable.
	priv, err := f.s.CreateConversation(f.ctx, "alice@x.com", "Private", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.BranchConversation(f.ctx, "bob@x.com", priv.ID, 1, "nope"); err == nil {
		t.Error("branching an unshared chat of someone else's must fail")
	}
}

// The counts the two destructive confirms quote.
func TestLeaveTeamAndProjectImpactCounts(t *testing.T) {
	f := newTeamFixture(t)
	// A second team-shared project, owned by BOB, so alice's leave-impact can
	// distinguish "projects I'd lose" from "projects I own".
	if _, err := f.s.CreateProject(f.ctx, &Project{
		OwnerEmail: "bob@x.com", Name: "Bob's", TeamID: "quant",
	}); err != nil {
		t.Fatal(err)
	}
	f.sharedChat(t, "alice@x.com", f.project.ID, "One")
	f.sharedChat(t, "bob@x.com", f.project.ID, "Two")
	if _, err := f.s.CreateProjectMemory(f.ctx, f.project.ID, "bob@x.com", "spreads are quoted in bps", "fact"); err != nil {
		t.Fatal(err)
	}

	impact, err := f.s.LeaveTeamImpact(f.ctx, "alice@x.com", "quant")
	if err != nil {
		t.Fatalf("LeaveTeamImpact: %v", err)
	}
	if impact.SharedProjects == nil || *impact.SharedProjects != 1 {
		t.Errorf("shared projects = %v, want 1 (bob's; alice keeps her own)", impact.SharedProjects)
	}
	if impact.SharedChats == nil || *impact.SharedChats != 1 {
		t.Errorf("shared chats = %v, want 1 (alice's own)", impact.SharedChats)
	}

	pi, err := f.s.ProjectImpact(f.ctx, f.project.ID)
	if err != nil {
		t.Fatalf("ProjectImpact: %v", err)
	}
	if pi.Memories != 1 || pi.Chats != 2 || pi.Members != 2 || pi.TeamSharedChats != 2 {
		t.Errorf("project impact = %+v, want 1 memory / 2 chats / 2 members / 2 shared", pi)
	}
}

// "Delete all unpinned" clears the Temporary list, and a project chat is not
// in it. Red/green: before the project_id filter this deleted filed chats,
// which made the "chats in a project don't expire" promise false.
func TestBulkDeleteSkipsProjectChats(t *testing.T) {
	for _, soft := range []bool{false, true} {
		name := "hard delete"
		if soft {
			name = "soft delete"
		}
		t.Run(name, func(t *testing.T) {
			f := newTeamFixture(t)
			f.s.SetSoftDelete(soft)

			loose, err := f.s.CreateConversation(f.ctx, "alice@x.com", "loose", "victoria", "", false)
			if err != nil {
				t.Fatal(err)
			}
			filed, err := f.s.CreateConversation(f.ctx, "alice@x.com", "filed", "victoria", "", false)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.s.SetConversationProject(f.ctx, "alice@x.com", filed.ID, f.project.ID); err != nil {
				t.Fatal(err)
			}

			n, err := f.s.DeleteAllUnpinned(f.ctx, "alice@x.com")
			if err != nil {
				t.Fatalf("DeleteAllUnpinned: %v", err)
			}
			if n != 1 {
				t.Errorf("deleted %d, want 1 (the unfiled chat only)", n)
			}
			if got, _ := f.s.Get(f.ctx, "alice@x.com", filed.ID); got == nil {
				t.Error("a project chat must survive Delete all unpinned")
			}
			if got, _ := f.s.Get(f.ctx, "alice@x.com", loose.ID); got != nil {
				t.Error("the unfiled chat should be gone")
			}

			// The label-filtered form keys the same way.
			filed2, err := f.s.CreateConversation(f.ctx, "alice@x.com", "filed2", "victoria", "", false)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.s.SetConversationProject(f.ctx, "alice@x.com", filed2.ID, f.project.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.DeleteAllMatching(f.ctx, "alice@x.com", ""); err != nil {
				t.Fatalf("DeleteAllMatching: %v", err)
			}
			if got, _ := f.s.Get(f.ctx, "alice@x.com", filed2.ID); got == nil {
				t.Error("a project chat must survive the filtered bulk delete too")
			}
		})
	}
}

// Team learnings: the project-scoped memory patch, the promotion of a personal
// memory, and accepting a proposal straight into the project.
func TestProjectMemoryManagement(t *testing.T) {
	f := newTeamFixture(t)

	m, err := f.s.CreateProjectMemory(f.ctx, f.project.ID, "bob@x.com", "quote spreads in bps", "fact")
	if err != nil {
		t.Fatalf("CreateProjectMemory: %v", err)
	}

	// Retire is the default remove: the entry stops being injected, the record
	// (and its author) survives.
	yes := true
	retired, err := f.s.UpdateProjectMemory(f.ctx, f.project.ID, m.ID, MemoryPatch{Retired: &yes})
	if err != nil {
		t.Fatalf("UpdateProjectMemory: %v", err)
	}
	if !retired.Retired() || retired.UserEmail != "bob@x.com" {
		t.Errorf("retired = %v, author = %q", retired.Retired(), retired.UserEmail)
	}

	// Scope isolation both ways: the personal API cannot touch a project row.
	if _, err := f.s.UpdateMemory(f.ctx, "bob@x.com", m.ID, MemoryPatch{Retired: &yes}); err == nil {
		t.Error("the personal memory API must not reach a project row")
	}

	// Promotion MOVES a personal memory in (no duplicate injection).
	personal, err := f.s.CreateMemory(f.ctx, "bob@x.com", "the desk closes at 16:00", "manual", "fact")
	if err != nil {
		t.Fatal(err)
	}
	moved, err := f.s.MoveMemoryToProject(f.ctx, "bob@x.com", personal.ID, f.project.ID)
	if err != nil {
		t.Fatalf("MoveMemoryToProject: %v", err)
	}
	if moved.ProjectID != f.project.ID {
		t.Errorf("moved memory project = %q", moved.ProjectID)
	}
	list, err := f.s.ListMemories(f.ctx, "bob@x.com")
	if err != nil {
		t.Fatal(err)
	}
	for _, pm := range list {
		if pm.ID == personal.ID {
			t.Error("a promoted memory must leave personal memory, not be copied")
		}
	}
	// It is not somebody else's to promote.
	other, err := f.s.CreateMemory(f.ctx, "alice@x.com", "alice's own", "manual", "fact")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.MoveMemoryToProject(f.ctx, "bob@x.com", other.ID, f.project.ID); err == nil {
		t.Error("promoting another user's memory must fail")
	}

	// A PENDING PROPOSAL is not promotable either (source <> 'proposed'). It is
	// not a memory yet — it is a question the user has not answered — and
	// moving it would publish to the whole team something they never accepted
	// even for themselves. The way to put a proposal in front of the team is
	// AcceptMemoryProposalIntoProject, below, which is an approval.
	pending, err := f.s.CreateMemoryProposal(f.ctx, "bob@x.com", "", MemoryProposalParams{
		Content: "bob is interviewing elsewhere", Kind: "fact",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.MoveMemoryToProject(f.ctx, "bob@x.com", pending.ID, f.project.ID); err == nil {
		t.Error("an unanswered proposal must not be promotable into a project")
	}

	// A proposal can be accepted straight into the project.
	prop, err := f.s.CreateMemoryProposal(f.ctx, "bob@x.com", "", MemoryProposalParams{
		Content: "settlement is T+1", Kind: "fact",
	})
	if err != nil {
		t.Fatalf("CreateMemoryProposal: %v", err)
	}
	accepted, err := f.s.AcceptMemoryProposalIntoProject(f.ctx, "bob@x.com", prop.ID, f.project.ID)
	if err != nil {
		t.Fatalf("AcceptMemoryProposalIntoProject: %v", err)
	}
	if accepted.ProjectID != f.project.ID || accepted.Source == "proposed" {
		t.Errorf("accepted = project %q source %q", accepted.ProjectID, accepted.Source)
	}
	shared, err := f.s.ListProjectMemories(f.ctx, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(shared) != 3 {
		t.Errorf("project memories = %d, want 3", len(shared))
	}
}

// Deleting a project takes its team learnings with it (they are project state)
// while the conversations survive, detached — the two halves the delete confirm
// promises.
func TestDeleteProjectDropsLearningsKeepsChats(t *testing.T) {
	f := newTeamFixture(t)
	if _, err := f.s.CreateProjectMemory(f.ctx, f.project.ID, "bob@x.com", "a learning", "fact"); err != nil {
		t.Fatal(err)
	}
	c := f.sharedChat(t, "bob@x.com", f.project.ID, "Bob's")

	if err := f.s.DeleteProject(f.ctx, "alice@x.com", f.project.ID); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if mems, _ := f.s.ListProjectMemories(f.ctx, f.project.ID); len(mems) != 0 {
		t.Errorf("team learnings must die with the project, got %d", len(mems))
	}
	got, _ := f.s.Get(f.ctx, "bob@x.com", c.ID)
	if got == nil || got.ProjectID != "" {
		t.Errorf("member chat after delete: %+v", got)
	}
	// Detached and unfiled, it is Temporary again — retention can reach it,
	// which is exactly what the confirm warns about.
	if _, err := f.s.db.ExecContext(f.ctx,
		`UPDATE conversations SET updated_at = $1 WHERE id = $2`,
		time.Now().Add(-30*24*time.Hour).Unix(), c.ID); err != nil {
		t.Fatal(err)
	}
	expired, _, err := f.s.SweepExpired(f.ctx, 14*24*time.Hour, 100)
	if err != nil {
		t.Fatal(err)
	}
	if expired != 1 {
		t.Errorf("a detached chat is temporary again: swept %d, want 1", expired)
	}
}

// Ownership transfer (ADR-0057). A project is owner-only to edit and delete,
// so before this "the owner left" was terminal: the definition froze and
// deleting the account destroyed the project and its team learnings.
func TestTransferProjectOwnership(t *testing.T) {
	f := newTeamFixture(t)
	if _, err := f.s.CreateProjectMemory(f.ctx, f.project.ID, "alice@x.com", "a learning", "fact"); err != nil {
		t.Fatal(err)
	}
	chat := f.sharedChat(t, "alice@x.com", f.project.ID, "Study")

	// Alice → Bob, a teammate.
	moved, err := f.s.TransferProjectOwnership(f.ctx, f.project.ID, "bob@x.com")
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if moved.OwnerEmail != "bob@x.com" {
		t.Errorf("owner = %q, want bob", moved.OwnerEmail)
	}
	// It changes WHO MAY EDIT, and nothing else.
	if moved.TeamID != "quant" || moved.Name != f.project.Name {
		t.Errorf("transfer must not touch the definition: %+v", moved)
	}
	if mems, _ := f.s.ListProjectMemories(f.ctx, f.project.ID); len(mems) != 1 {
		t.Error("team learnings must survive a transfer")
	}
	if got, _ := f.s.Get(f.ctx, "alice@x.com", chat.ID); got == nil || !got.TeamVisible {
		t.Error("members' chats and their shares must survive a transfer")
	}

	// The new owner can now edit; the old one cannot.
	name := "Renamed by Bob"
	if _, err := f.s.UpdateProject(f.ctx, "bob@x.com", f.project.ID, ProjectPatch{Name: &name}); err != nil {
		t.Errorf("new owner must be able to edit: %v", err)
	}
	if _, err := f.s.UpdateProject(f.ctx, "alice@x.com", f.project.ID, ProjectPatch{Name: &name}); err == nil {
		t.Error("the previous owner must lose edit rights")
	}
	// Alice is still a member (same team), so she keeps using the project.
	if list, _ := f.s.ListProjectsForUser(f.ctx, "alice@x.com", "quant"); len(list) != 1 {
		t.Error("the previous owner stays a member via the team")
	}

	// Handing a team-shared project outside its team is refused — it would be
	// shared with a team its owner is not in.
	if _, err := f.s.TransferProjectOwnership(f.ctx, f.project.ID, "dana@x.com"); !errors.Is(err, ErrNotAProjectMember) {
		t.Errorf("cross-team transfer: got %v, want ErrNotAProjectMember", err)
	}
	// An unknown account is refused with the SAME sentinel as a real account
	// in the wrong team. Distinguishing them turned the route into an
	// account-existence oracle over arbitrary addresses.
	if _, err := f.s.TransferProjectOwnership(f.ctx, f.project.ID, "ghost@x.com"); !errors.Is(err, ErrNotAProjectMember) {
		t.Errorf("unknown target: got %v, want ErrNotAProjectMember (indistinguishable from a wrong-team target)", err)
	}
	// A PERSONAL project cannot be transferred at all: there is no membership
	// to check, so it would be a way to push a project — with instructions of
	// the sender's choosing, injected into every chat started in it — into a
	// stranger's rail without asking them.
	personal, err := f.s.CreateProject(f.ctx, &Project{OwnerEmail: "alice@x.com", Name: "Alice's own"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.TransferProjectOwnership(f.ctx, personal.ID, "bob@x.com"); !errors.Is(err, ErrNotAProjectMember) {
		t.Errorf("personal transfer: got %v, want ErrNotAProjectMember", err)
	}
	// Re-transferring to the current owner is an idempotent no-op.
	if _, err := f.s.TransferProjectOwnership(f.ctx, f.project.ID, "bob@x.com"); err != nil {
		t.Errorf("idempotent re-transfer: %v", err)
	}
}

// Deleting an account that still owns a TEAM-SHARED project used to destroy
// that project and every team learning in it — for people who are still here.
// It now fails closed and names what to transfer first.
func TestDeleteUserRefusesToTakeTeamSharedProjects(t *testing.T) {
	f := newTeamFixture(t)
	if _, err := f.s.CreateProjectMemory(f.ctx, f.project.ID, "bob@x.com", "a learning", "fact"); err != nil {
		t.Fatal(err)
	}

	err := f.s.DeleteUser(f.ctx, "alice@x.com")
	var owns *OwnsSharedProjectsError
	if !errors.As(err, &owns) {
		t.Fatalf("delete: got %v, want OwnsSharedProjectsError", err)
	}
	if len(owns.Projects) != 1 || owns.Projects[0] != "Quant" {
		t.Errorf("named projects = %v, want [Quant]", owns.Projects)
	}
	// Nothing was destroyed on the way to that refusal.
	if p, _ := f.s.GetProject(f.ctx, f.project.ID); p == nil {
		t.Fatal("the refused delete must not have removed the project")
	}
	if mems, _ := f.s.ListProjectMemories(f.ctx, f.project.ID); len(mems) != 1 {
		t.Error("the refused delete must not have removed team learnings")
	}
	if u, err := f.s.GetUser(f.ctx, "alice@x.com"); err != nil || u == nil {
		t.Error("the refused delete must not have removed the account")
	}

	// Transfer, then the delete goes through.
	if _, err := f.s.TransferProjectOwnership(f.ctx, f.project.ID, "bob@x.com"); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DeleteUser(f.ctx, "alice@x.com"); err != nil {
		t.Fatalf("delete after transfer: %v", err)
	}
	if p, _ := f.s.GetProject(f.ctx, f.project.ID); p == nil {
		t.Error("the team keeps its project after the owner's account is deleted")
	}
	if mems, _ := f.s.ListProjectMemories(f.ctx, f.project.ID); len(mems) != 1 {
		t.Error("the team keeps its learnings too")
	}
}

// A PERSONAL project still goes with the account: nobody else can see it, so
// there is nothing to hand over and no one to lose it.
func TestDeleteUserStillTakesPersonalProjects(t *testing.T) {
	f := newTeamFixture(t)
	personal, err := f.s.CreateProject(f.ctx, &Project{OwnerEmail: "carol@x.com", Name: "Carol's"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.DeleteUser(f.ctx, "carol@x.com"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if p, _ := f.s.GetProject(f.ctx, personal.ID); p != nil {
		t.Error("a personal project belongs with its account")
	}
}

// ProjectMemberEmails is the transfer picker's options: the team, plus the
// owner (who is a member whether or not there is a team).
func TestProjectMemberEmails(t *testing.T) {
	f := newTeamFixture(t)
	got, err := f.s.ProjectMemberEmails(f.ctx, f.project.ID)
	if err != nil {
		t.Fatalf("members: %v", err)
	}
	want := []string{"alice@x.com", "bob@x.com"}
	if len(got) != len(want) {
		t.Fatalf("members = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("members = %v, want %v", got, want)
		}
	}

	// A personal project has exactly one: its owner.
	personal, err := f.s.CreateProject(f.ctx, &Project{OwnerEmail: "carol@x.com", Name: "Solo"})
	if err != nil {
		t.Fatal(err)
	}
	solo, err := f.s.ProjectMemberEmails(f.ctx, personal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(solo) != 1 || solo[0] != "carol@x.com" {
		t.Errorf("personal project members = %v", solo)
	}
}

// A share names its audience, and that audience cannot be changed out from
// under the owner.
//
// Red/green: `team_visible` was a bare boolean, and every read inferred the
// audience from the owner's CURRENT users.team_id. So an admin moving the
// owner to another team silently handed every chat they had shared to the new
// team — a group the owner never chose, and never saw a prompt about. Before
// migration 054 both assertions below failed: a stranger in the new team could
// list AND read the chat.
func TestTeamShareAudienceIsStampedNotInferred(t *testing.T) {
	f := newTeamFixture(t)
	c := f.sharedChat(t, "bob@x.com", f.project.ID, "Quant secret")

	// Sanity: while bob is in quant, his teammate alice can read it.
	if snap, _ := f.s.GetTeamVisibleConversation(f.ctx, "alice@x.com", c.ID); snap == nil {
		t.Fatal("a teammate must be able to read a chat shared with their team")
	}

	// An admin moves bob quant → ops (Settings → Admin → Users). This is a
	// DIFFERENT code path from the self-serve leave, and used to skip every
	// unshare.
	ops := "ops"
	if _, err := f.s.SetUserRoleTeam(f.ctx, "bob@x.com", nil, &ops); err != nil {
		t.Fatalf("admin team move: %v", err)
	}

	// dana is in ops and has nothing to do with quant.
	list, err := f.s.ListTeamConversations(f.ctx, "dana@x.com")
	if err != nil {
		t.Fatalf("ListTeamConversations: %v", err)
	}
	for _, conv := range list {
		if conv.ID == c.ID {
			t.Error("a chat shared with quant must never be listed to the owner's NEW team")
		}
	}
	if snap, _ := f.s.GetTeamVisibleConversation(f.ctx, "dana@x.com", c.ID); snap != nil {
		t.Error("a chat shared with quant must never be readable by the owner's NEW team")
	}

	// And the move revoked it for quant too, because the owner left — the same
	// rule the self-serve Leave confirm promises, now on the admin path.
	if snap, _ := f.s.GetTeamVisibleConversation(f.ctx, "alice@x.com", c.ID); snap != nil {
		t.Error("moving the owner out of a team must unshare the chats they shared with it")
	}
	if got, _ := f.s.Get(f.ctx, "bob@x.com", c.ID); got.TeamVisible {
		t.Error("the flag itself must be cleared, not just hidden by the read gate")
	}
}

// Sharing needs BOTH an audience to name and a place for that audience to find
// the chat, and the store refuses rather than storing half of it.
//
// Red/green: a refusal that is only a UI narrowing is not a rule. Every
// revocation path (leaving the team, unsharing the project, deleting it) is
// keyed on the PROJECT, so a flag set on a project-less chat — reachable
// through the documented ADR-0013 endpoint — matched none of them, and the
// Share dialog disables its own toggle in exactly that state. The share was
// live to the whole team, listed by ?scope=team, readable by id, and its owner
// had no surface anywhere that could take it back.
func TestTeamShareNeedsAnAudienceAndAHome(t *testing.T) {
	// carol is teamless: no audience to name.
	t.Run("no team", func(t *testing.T) {
		f := newTeamFixture(t)
		personal, err := f.s.CreateProject(f.ctx, &Project{OwnerEmail: "carol@x.com", Name: "Carol's"})
		if err != nil {
			t.Fatal(err)
		}
		c, err := f.s.CreateConversation(f.ctx, "carol@x.com", "Solo", "victoria", "", false)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.s.SetConversationProject(f.ctx, "carol@x.com", c.ID, personal.ID); err != nil {
			t.Fatal(err)
		}
		if stored, err := f.s.SetConversationTeamVisible(f.ctx, "carol@x.com", c.ID, true); !errors.Is(err, ErrNoTeamToShareWith) || stored {
			t.Fatalf("share by a teamless owner = (%v, %v), want (false, ErrNoTeamToShareWith)", stored, err)
		}
		if got, _ := f.s.Get(f.ctx, "carol@x.com", c.ID); got.TeamVisible {
			t.Error("a teamless owner has no audience to share with; the flag must stay FALSE")
		}
		if snap, _ := f.s.GetTeamVisibleConversation(f.ctx, "dana@x.com", c.ID); snap != nil {
			t.Error("nobody may read a chat with no named audience")
		}
	})

	// alice has a team, but the chat has nowhere for it to appear.
	for _, tc := range []struct {
		name string
		home func(t *testing.T, f teamFixture) string // returns the project id to file into ("" = none)
	}{
		{"no project", func(_ *testing.T, _ teamFixture) string { return "" }},
		{"a personal project", func(t *testing.T, f teamFixture) string {
			p, err := f.s.CreateProject(f.ctx, &Project{OwnerEmail: "alice@x.com", Name: "Mine"})
			if err != nil {
				t.Fatal(err)
			}
			return p.ID
		}},
		{"another team's project", func(t *testing.T, f teamFixture) string {
			p, err := f.s.CreateProject(f.ctx, &Project{OwnerEmail: "dana@x.com", Name: "Ops", TeamID: "ops"})
			if err != nil {
				t.Fatal(err)
			}
			return p.ID
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTeamFixture(t)
			c, err := f.s.CreateConversation(f.ctx, "alice@x.com", "Homeless", "victoria", "", false)
			if err != nil {
				t.Fatal(err)
			}
			if pid := tc.home(t, f); pid != "" {
				if err := f.s.SetConversationProject(f.ctx, "alice@x.com", c.ID, pid); err != nil {
					t.Fatal(err)
				}
			}
			if stored, err := f.s.SetConversationTeamVisible(f.ctx, "alice@x.com", c.ID, true); !errors.Is(err, ErrNoTeamShareHome) || stored {
				t.Fatalf("share with no home = (%v, %v), want (false, ErrNoTeamShareHome)", stored, err)
			}
			if got, _ := f.s.Get(f.ctx, "alice@x.com", c.ID); got.TeamVisible {
				t.Error("the flag must not be set for a chat with nowhere to appear")
			}
			// And a teammate cannot reach it by either door.
			if snap, _ := f.s.GetTeamVisibleConversation(f.ctx, "bob@x.com", c.ID); snap != nil {
				t.Error("a chat with no home must not be readable")
			}
			list, _ := f.s.ListTeamConversations(f.ctx, "bob@x.com")
			for _, conv := range list {
				if conv.ID == c.ID {
					t.Error("a chat with no home must not be listed")
				}
			}
		})
	}

	// Revocation is never refused, from any state — including one a pre-054
	// client left behind.
	t.Run("unsharing always works", func(t *testing.T) {
		f := newTeamFixture(t)
		c := f.sharedChat(t, "alice@x.com", f.project.ID, "Study")
		// Force the row into the legacy shape the enforcement now rejects.
		if _, err := f.s.db.ExecContext(f.ctx,
			`UPDATE conversations SET project_id = NULL WHERE id = $1`, c.ID); err != nil {
			t.Fatal(err)
		}
		if stored, err := f.s.SetConversationTeamVisible(f.ctx, "alice@x.com", c.ID, false); err != nil || stored {
			t.Fatalf("un-share = (%v, %v), want (false, nil)", stored, err)
		}
	})
}

// Every path that clears team_visible must clear the stamped audience with it,
// so "not shared" has exactly one representation and a later re-share cannot
// resurrect an old audience.
func TestUnsharingClearsTheStampedAudience(t *testing.T) {
	// Each case gets its own fixture — newTestStore truncates the tables, so a
	// closure that captured an outer fixture would act on rows that no longer
	// exist. The fixture is therefore a parameter, not a capture.
	for _, tc := range []struct {
		name  string
		unset func(t *testing.T, f teamFixture, c *Conversation)
	}{
		{"owner opts out", func(t *testing.T, f teamFixture, c *Conversation) {
			if _, err := f.s.SetConversationTeamVisible(f.ctx, "alice@x.com", c.ID, false); err != nil {
				t.Fatal(err)
			}
		}},
		{"chat leaves the project", func(t *testing.T, f teamFixture, c *Conversation) {
			if err := f.s.SetConversationProject(f.ctx, "alice@x.com", c.ID, ""); err != nil {
				t.Fatal(err)
			}
		}},
		{"project stops being team-shared", func(t *testing.T, f teamFixture, _ *Conversation) {
			none := ""
			if _, err := f.s.UpdateProject(f.ctx, "alice@x.com", f.project.ID, ProjectPatch{TeamID: &none}); err != nil {
				t.Fatal(err)
			}
		}},
		{"the project is deleted", func(t *testing.T, f teamFixture, _ *Conversation) {
			if err := f.s.DeleteProject(f.ctx, "alice@x.com", f.project.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"the owner leaves the team", func(t *testing.T, f teamFixture, _ *Conversation) {
			if _, err := f.s.SetOwnTeam(f.ctx, "alice@x.com", "", false); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTeamFixture(t)
			c := f.sharedChat(t, "alice@x.com", f.project.ID, "Study")
			tc.unset(t, f, c)
			var visible bool
			var audience *string
			if err := f.s.db.QueryRowContext(f.ctx,
				`SELECT team_visible, team_shared_with FROM conversations WHERE id = $1`, c.ID,
			).Scan(&visible, &audience); err != nil {
				t.Fatal(err)
			}
			if visible || audience != nil {
				t.Errorf("after unshare: team_visible=%v team_shared_with=%v, want false/NULL", visible, audience)
			}
		})
	}
}

// A teammate's BRANCH may copy no more than team-view showed them.
//
// Red/green: GetTeamVisibleConversation strips tool_call / tool_result /
// reasoning because their content carries command output and API responses the
// owner never shared. BranchConversation then copied `id <= branchPoint`
// verbatim into a conversation the BRANCHER OWNS and can read in full — so the
// redaction was one click away from decorative: open the chat you can only see
// the prose of, hit Branch, read your own copy.
func TestBranchFromATeammateCopiesOnlyWhatTeamViewShows(t *testing.T) {
	f := newTeamFixture(t)
	c := f.sharedChat(t, "alice@x.com", f.project.ID, "Spread study")
	// A user turn carrying an image that lives in ALICE'S workspace. The agent
	// re-reads these paths verbatim on the next turn, with no ownership check.
	if _, err := f.s.AppendHistory(f.ctx, c.ID, []agent.HistoryEntry{
		{Role: "user", Type: "text", Content: []byte(`{"text":"and this chart?","images":[{"path":"/w/alice/secret.png","name":"secret.png"}]}`)},
	}); err != nil {
		t.Fatal(err)
	}

	full, err := f.s.LoadHistory(f.ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := full[len(full)-1].ID

	branch, err := f.s.BranchConversation(f.ctx, "bob@x.com", c.ID, last, "Bob's fork")
	if err != nil {
		t.Fatalf("branch: %v", err)
	}
	copied, err := f.s.LoadHistory(f.ctx, branch.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range copied {
		if m.Type != "text" || (m.Role != "user" && m.Role != "assistant") {
			t.Errorf("branch copied a %s/%s entry a teammate was never shown", m.Role, m.Type)
		}
		if strings.Contains(string(m.Content), "/w/alice/") {
			t.Errorf("branch copied a path into the owner's workspace: %s", m.Content)
		}
		if strings.Contains(string(m.Content), "etc/passwd") {
			t.Errorf("branch copied tool output: %s", m.Content)
		}
	}
	if len(copied) == 0 {
		t.Fatal("the branch must still carry the prose")
	}

	// The OWNER's own branch is unchanged: they may read their own history in
	// full, so nothing is filtered on that path.
	mine, err := f.s.BranchConversation(f.ctx, "alice@x.com", c.ID, last, "Alice's fork")
	if err != nil {
		t.Fatalf("owner branch: %v", err)
	}
	own, _ := f.s.LoadHistory(f.ctx, mine.ID)
	var sawTool bool
	for _, m := range own {
		if m.Type == "tool_call" {
			sawTool = true
		}
	}
	if !sawTool {
		t.Error("the owner's own branch must keep the full history")
	}
}

// Lockdown is a property of the conversation, and a fork of a locked-down chat
// is locked down — including across users.
//
// Red/green: the teammate path synthesized its parent from
// TeamSharedConversation, which had no Lockdown field, so the flag read false
// and the fork ran with normal network egress and no model restriction while
// carrying the locked-down chat's history.
func TestBranchFromATeammateKeepsLockdown(t *testing.T) {
	f := newTeamFixture(t)
	c, err := f.s.CreateConversation(f.ctx, "alice@x.com", "Sealed", "victoria", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Lockdown {
		t.Fatal("fixture: expected a lockdown conversation")
	}
	if err := f.s.SetConversationProject(f.ctx, "alice@x.com", c.ID, f.project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AppendHistory(f.ctx, c.ID, []agent.HistoryEntry{
		{Role: "user", Type: "text", Content: []byte(`{"text":"hi"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetConversationTeamVisible(f.ctx, "alice@x.com", c.ID, true); err != nil {
		t.Fatal(err)
	}
	msgs, _ := f.s.LoadHistory(f.ctx, c.ID)
	fork, err := f.s.BranchConversation(f.ctx, "bob@x.com", c.ID, msgs[0].ID, "fork")
	if err != nil {
		t.Fatalf("branch: %v", err)
	}
	if !fork.Lockdown {
		t.Error("a fork of a locked-down chat must stay locked down")
	}
	stored, _ := f.s.Get(f.ctx, "bob@x.com", fork.ID)
	if stored == nil || !stored.Lockdown {
		t.Error("lockdown must be persisted on the fork, not just returned")
	}
}

// The pairing asks WHICH team a project is shared with, not merely whether it
// is shared with one.
//
// Red/green: both write paths tested `team_id <> ”`, so a chat shared with
// `quant` could be moved into an `ops` project and keep its `quant` stamp —
// readable by quant, listed in nobody's Team section (that matches on the
// caller's team, which is now ops), and revocable from no surface. Likewise a
// project re-shared with a different team left the old audience reading it.
func TestPairingComparesTheTeamNotJustThePresenceOfOne(t *testing.T) {
	assertUnshared := func(t *testing.T, f teamFixture, id string) {
		t.Helper()
		var visible bool
		var audience *string
		if err := f.s.db.QueryRowContext(f.ctx,
			`SELECT team_visible, team_shared_with FROM conversations WHERE id = $1`, id,
		).Scan(&visible, &audience); err != nil {
			t.Fatal(err)
		}
		if visible || audience != nil {
			t.Errorf("team_visible=%v team_shared_with=%v, want false/NULL", visible, audience)
		}
	}

	t.Run("refiled into another team's project", func(t *testing.T) {
		f := newTeamFixture(t)
		c := f.sharedChat(t, "alice@x.com", f.project.ID, "Study")
		ops, err := f.s.CreateProject(f.ctx, &Project{OwnerEmail: "dana@x.com", Name: "Ops", TeamID: "ops"})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.s.SetConversationProject(f.ctx, "alice@x.com", c.ID, ops.ID); err != nil {
			t.Fatal(err)
		}
		assertUnshared(t, f, c.ID)
	})

	t.Run("project re-shared with another team", func(t *testing.T) {
		f := newTeamFixture(t)
		c := f.sharedChat(t, "alice@x.com", f.project.ID, "Study")
		ops := "ops"
		if _, err := f.s.UpdateProject(f.ctx, "alice@x.com", f.project.ID, ProjectPatch{TeamID: &ops}); err != nil {
			t.Fatal(err)
		}
		assertUnshared(t, f, c.ID)
	})
}

// Detaching a chat from a project must not make it instantly reapable.
//
// Red/green: project_id IS NULL is exactly what re-arms the TTL sweep and the
// unpinned cap, and DeleteProject detached every member's chats WITHOUT
// touching updated_at. A four-month-old chat therefore became sweep-eligible
// the moment the owner deleted the project — hard-deleted on the next turn any
// user took, with no window in which to pin it, while the confirm promised
// members their chats would "expire unless pinned".
func TestDeletingAProjectGivesMembersAFullRetentionWindow(t *testing.T) {
	f := newTeamFixture(t)
	c, err := f.s.CreateConversation(f.ctx, "bob@x.com", "Old work", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetConversationProject(f.ctx, "bob@x.com", c.ID, f.project.ID); err != nil {
		t.Fatal(err)
	}
	ancient := time.Now().Add(-120 * 24 * time.Hour).Unix()
	if _, err := f.s.db.ExecContext(f.ctx,
		`UPDATE conversations SET updated_at = $1 WHERE id = $2`, ancient, c.ID); err != nil {
		t.Fatal(err)
	}

	if err := f.s.DeleteProject(f.ctx, "alice@x.com", f.project.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.s.SweepExpired(f.ctx, 14*24*time.Hour, 50); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.s.Get(f.ctx, "bob@x.com", c.ID); got == nil {
		t.Fatal("a member's chat was hard-deleted by the very sweep the project had been exempting it from")
	}
}

// filingState reads the two things the filing invariant is about, for one chat:
// whether it is still filed in a project, and whether it is still team-shared.
// It reads the row directly (not Get) so a chat filed in a project nobody can
// see is still observable — which is the whole bug this pins.
func filingState(t *testing.T, f teamFixture, convID string) (projectID string, shared bool) {
	t.Helper()
	var pid *string
	if err := f.s.db.QueryRowContext(f.ctx,
		`SELECT project_id, team_visible FROM conversations WHERE id = $1 AND deleted_at IS NULL`,
		convID,
	).Scan(&pid, &shared); err != nil {
		t.Fatalf("read filing state of %s: %v", convID, err)
	}
	if pid != nil {
		projectID = *pid
	}
	return projectID, shared
}

// listedByItsOwner reports whether the chat appears in its owner's own rail
// listing — the check that "unfiled" means "back in Temporary" rather than
// "gone".
func listedByItsOwner(t *testing.T, f teamFixture, owner, convID string) bool {
	t.Helper()
	list, err := f.s.List(f.ctx, owner, false)
	if err != nil {
		t.Fatalf("List(%s): %v", owner, err)
	}
	for _, c := range list {
		if c.ID == convID {
			return true
		}
	}
	return false
}

// Every path that takes a member's access to a project away UNFILES their
// chats — it never deletes them, and never leaves them filed in a project only
// somebody else can see.
//
// Red/green: the rail lists chats through the projects the CALLER can see, so a
// chat left filed in a project they cannot see is rendered by nothing — not
// Projects, not Temporary, not Archived. Unticking "Share with my team" on a
// project therefore made every teammate's chats in it VANISH from their own
// rail (re-ticking brought them back, which is how we know they were hidden
// rather than deleted). Each case below unshares, and each must also unfile.
func TestRevokingProjectAccessUnfilesInsteadOfHiding(t *testing.T) {
	const (
		alice = "alice@x.com" // owner of the fixture's team-shared project
		bob   = "bob@x.com"   // a teammate with a chat filed in it
	)
	for _, tc := range []struct {
		name string
		// revoke performs the access-removing act; chats maps a chat owner's
		// email to their chat id in the project.
		revoke func(t *testing.T, f teamFixture, chats map[string]string)
		// wantFiled / wantShared, per chat owner, AFTER the revocation.
		wantFiled  map[string]bool
		wantShared map[string]bool
	}{
		{
			name: "the owner unticks Share with my team",
			revoke: func(t *testing.T, f teamFixture, chats map[string]string) {
				personal := ""
				if _, err := f.s.UpdateProject(f.ctx, alice, f.project.ID, ProjectPatch{TeamID: &personal}); err != nil {
					t.Fatal(err)
				}
			},
			// Alice keeps her own chats where they are — she still owns the
			// project — and loses only the share.
			wantFiled:  map[string]bool{alice: true, bob: false},
			wantShared: map[string]bool{alice: false, bob: false},
		},
		{
			name: "the project is re-pointed at another team",
			revoke: func(t *testing.T, f teamFixture, chats map[string]string) {
				ops := "ops"
				if _, err := f.s.UpdateProject(f.ctx, alice, f.project.ID, ProjectPatch{TeamID: &ops}); err != nil {
					t.Fatal(err)
				}
			},
			wantFiled:  map[string]bool{alice: true, bob: false},
			wantShared: map[string]bool{alice: false, bob: false},
		},
		{
			name: "the project is deleted",
			revoke: func(t *testing.T, f teamFixture, chats map[string]string) {
				if err := f.s.DeleteProject(f.ctx, alice, f.project.ID); err != nil {
					t.Fatal(err)
				}
			},
			// Nobody can see a project that no longer exists, the owner
			// included: every chat is unfiled. This path already behaved this
			// way; it is the behavior the others were brought into line with.
			wantFiled:  map[string]bool{alice: false, bob: false},
			wantShared: map[string]bool{alice: false, bob: false},
		},
		{
			name: "the teammate leaves the team",
			revoke: func(t *testing.T, f teamFixture, chats map[string]string) {
				if _, err := f.s.SetOwnTeam(f.ctx, bob, "", false); err != nil {
					t.Fatal(err)
				}
			},
			// Alice is untouched by someone else leaving: her chat stays filed
			// AND stays shared with the team she named.
			wantFiled:  map[string]bool{alice: true, bob: false},
			wantShared: map[string]bool{alice: true, bob: false},
		},
		{
			name: "an admin moves the teammate to another team",
			revoke: func(t *testing.T, f teamFixture, chats map[string]string) {
				ops := "ops"
				if _, err := f.s.SetUserRoleTeam(f.ctx, bob, nil, &ops); err != nil {
					t.Fatal(err)
				}
			},
			wantFiled:  map[string]bool{alice: true, bob: false},
			wantShared: map[string]bool{alice: true, bob: false},
		},
		{
			name: "the owner is moved out of the team, then hands the project over",
			revoke: func(t *testing.T, f teamFixture, chats map[string]string) {
				// Alice keeps access while she owns it, even from another team…
				ops := "ops"
				if _, err := f.s.SetUserRoleTeam(f.ctx, alice, nil, &ops); err != nil {
					t.Fatal(err)
				}
				if pid, _ := filingState(t, f, chats[alice]); pid != f.project.ID {
					t.Fatalf("the owner must keep her own project's chats filed: project_id = %q", pid)
				}
				// …and loses it the moment the project is someone else's.
				if _, err := f.s.TransferProjectOwnership(f.ctx, f.project.ID, bob); err != nil {
					t.Fatal(err)
				}
			},
			wantFiled:  map[string]bool{alice: false, bob: true},
			wantShared: map[string]bool{alice: false, bob: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTeamFixture(t)
			chats := map[string]string{
				alice: f.sharedChat(t, alice, f.project.ID, "Alice's study").ID,
				bob:   f.sharedChat(t, bob, f.project.ID, "Bob's branch").ID,
			}

			tc.revoke(t, f, chats)

			for _, owner := range []string{alice, bob} {
				convID := chats[owner]
				pid, shared := filingState(t, f, convID)
				wantPID := ""
				if tc.wantFiled[owner] {
					wantPID = f.project.ID
				}
				if pid != wantPID {
					t.Errorf("%s: project_id = %q, want %q", owner, pid, wantPID)
				}
				if shared != tc.wantShared[owner] {
					t.Errorf("%s: team_visible = %v, want %v", owner, shared, tc.wantShared[owner])
				}
				// Nothing is ever deleted to satisfy the invariant, and an
				// unfiled chat is back in its owner's Temporary list — not
				// hidden behind a project they cannot see.
				if got, err := f.s.Get(f.ctx, owner, convID); err != nil || got == nil {
					t.Fatalf("%s: the chat must still exist (err=%v)", owner, err)
				}
				if !listedByItsOwner(t, f, owner, convID) {
					t.Errorf("%s: the chat is missing from its own owner's listing", owner)
				}
			}
		})
	}
}

// The counts the untick confirm quotes: how many chats from teammates the
// project holds, and how many people they belong to — and that acting on the
// project unfiles exactly those.
func TestProjectImpactCountsChatsFromTeammates(t *testing.T) {
	f := newTeamFixture(t)
	mine := f.sharedChat(t, "alice@x.com", f.project.ID, "Mine")
	bobs := f.sharedChat(t, "bob@x.com", f.project.ID, "Bob's")
	bobs2 := f.sharedChat(t, "bob@x.com", f.project.ID, "Bob's second")
	// Dana is in another team: a chat of hers filed here is the state an admin
	// team move leaves behind, and it counts as a teammate's chat either way —
	// what the confirm promises is "these move to their unfiled chats".
	danas, err := f.s.CreateConversation(f.ctx, "dana@x.com", "Dana's", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetConversationProject(f.ctx, "dana@x.com", danas.ID, f.project.ID); err != nil {
		t.Fatal(err)
	}

	impact, err := f.s.ProjectImpact(f.ctx, f.project.ID)
	if err != nil {
		t.Fatalf("ProjectImpact: %v", err)
	}
	if impact.Chats != 4 || impact.Members != 3 {
		t.Errorf("impact = %+v, want 4 chats / 3 members", impact)
	}
	if impact.ChatsFromTeammates != 3 || impact.TeammatesWithChats != 2 {
		t.Errorf("teammate counts = %d chats / %d members, want 3 / 2",
			impact.ChatsFromTeammates, impact.TeammatesWithChats)
	}

	// The number is not decoration: unticking moves exactly those chats.
	personal := ""
	if _, err := f.s.UpdateProject(f.ctx, "alice@x.com", f.project.ID, ProjectPatch{TeamID: &personal}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{bobs.ID, bobs2.ID, danas.ID} {
		if pid, _ := filingState(t, f, id); pid != "" {
			t.Errorf("chat %s stayed filed in a project its owner cannot see", id)
		}
	}
	if pid, _ := filingState(t, f, mine.ID); pid != f.project.ID {
		t.Errorf("the owner's own chat must not move: project_id = %q", pid)
	}
	after, err := f.s.ProjectImpact(f.ctx, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ChatsFromTeammates != 0 || after.TeammatesWithChats != 0 {
		t.Errorf("after the untick, teammate counts = %+v, want zeroes", after)
	}
}

// embeddedMigrationSQL returns the SQL of one embedded migration by version.
func embeddedMigrationSQL(t *testing.T, version int) string {
	t.Helper()
	ms, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range ms {
		if m.version == version {
			return m.sql
		}
	}
	t.Fatalf("migration %03d is not embedded", version)
	return ""
}

// Migration 055 repairs the rows the pre-fix write paths stranded: a chat filed
// in a project its owner cannot see is unfiled, never deleted.
//
// The stranded state is seeded directly (the fixed paths can no longer produce
// it) and the migration is applied INSIDE a transaction that is rolled back, so
// the shared test database keeps its already-migrated schema — the same
// technique TestMigration046_NormalizesLegacyPositionTies uses.
func TestMigration055UnfilesChatsTheirOwnersCannotSee(t *testing.T) {
	f := newTeamFixture(t)
	stranded := f.sharedChat(t, "bob@x.com", f.project.ID, "Bob's branch")
	owners := f.sharedChat(t, "alice@x.com", f.project.ID, "Alice's study")
	ancient := time.Now().Add(-90 * 24 * time.Hour).Unix()
	if _, err := f.s.db.ExecContext(f.ctx,
		`UPDATE conversations SET updated_at = $1 WHERE id = $2`, ancient, stranded.ID); err != nil {
		t.Fatal(err)
	}
	// The pre-fix leave-team: unshared, still filed, and its owner no longer in
	// the team that made the project visible to them.
	if _, err := f.s.db.ExecContext(f.ctx,
		`UPDATE users SET team_id = NULL WHERE email = 'bob@x.com'`); err != nil {
		t.Fatal(err)
	}
	if pid, _ := filingState(t, f, stranded.ID); pid != f.project.ID {
		t.Fatalf("fixture: the chat should start out stranded, project_id = %q", pid)
	}

	tx, err := f.s.db.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	sqlText := embeddedMigrationSQL(t, 55)
	res, err := tx.ExecContext(f.ctx, sqlText)
	if err != nil {
		t.Fatalf("apply migration 055: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Errorf("migration touched %d rows, want 1 (the stranded chat only)", n)
	}
	// Re-running it must be a no-op: every row it repaired now has a NULL
	// project_id, so nothing matches a second time.
	res, err = tx.ExecContext(f.ctx, sqlText)
	if err != nil {
		t.Fatalf("re-apply migration 055: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 0 {
		t.Errorf("re-applying the migration touched %d rows, want 0 (it must be idempotent)", n)
	}

	var pid *string
	var visible bool
	var audience *string
	var updatedAt int64
	if err := tx.QueryRowContext(f.ctx,
		`SELECT project_id, team_visible, team_shared_with, updated_at
		   FROM conversations WHERE id = $1`, stranded.ID,
	).Scan(&pid, &visible, &audience, &updatedAt); err != nil {
		t.Fatal(err)
	}
	if pid != nil || visible || audience != nil {
		t.Errorf("stranded chat: project_id=%v team_visible=%v team_shared_with=%v, want NULL/false/NULL",
			pid, visible, audience)
	}
	// The chat survives, and its retention clock restarts — an unfiled chat is
	// sweep-eligible, so keeping the old timestamp would have made a
	// months-old chat reapable on the next turn, with no window to pin it.
	if updatedAt <= ancient {
		t.Errorf("updated_at = %d, want a bump past %d so the TTL window restarts", updatedAt, ancient)
	}
	var n int
	if err := tx.QueryRowContext(f.ctx,
		`SELECT count(*) FROM conversations WHERE id = $1`, stranded.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Error("the migration must unfile the chat, never delete it")
	}

	// The owner's own chat is not touched at all: she can see her project, so
	// neither its filing nor its share is the migration's business.
	if err := tx.QueryRowContext(f.ctx,
		`SELECT project_id, team_visible FROM conversations WHERE id = $1`, owners.ID,
	).Scan(&pid, &visible); err != nil {
		t.Fatal(err)
	}
	if pid == nil || *pid != f.project.ID || !visible {
		t.Errorf("the owner's chat = project_id %v / team_visible %v, want it left alone", pid, visible)
	}
}
