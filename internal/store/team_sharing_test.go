package store

import (
	"context"
	"errors"
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
	if err := f.s.SetConversationTeamVisible(f.ctx, email, c.ID, true); err != nil {
		t.Fatalf("SetConversationTeamVisible: %v", err)
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
	if err := f.s.SetConversationTeamVisible(f.ctx, "alice@x.com", c.ID, false); err != nil {
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
	if err := f.s.SetConversationTeamVisible(f.ctx, "alice@x.com", c.ID, false); err != nil {
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
	if impact.SharedProjects != 1 {
		t.Errorf("shared projects = %d, want 1 (bob's; alice keeps her own)", impact.SharedProjects)
	}
	if impact.SharedChats != 1 {
		t.Errorf("shared chats = %d, want 1 (alice's own)", impact.SharedChats)
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
	// An unknown account is refused too.
	if _, err := f.s.TransferProjectOwnership(f.ctx, f.project.ID, "ghost@x.com"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("unknown target: got %v, want ErrUserNotFound", err)
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
