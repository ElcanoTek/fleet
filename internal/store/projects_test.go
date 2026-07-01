package store

import (
	"context"
	"testing"
)

// #509 projects: CRUD, team-membership visibility, owner-only mutation,
// delete-detach semantics, and the project-vs-personal memory scope split.
func TestProjectLifecycleAndMembership(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Users: alice+bob share a team; carol is teamless.
	for _, u := range []struct{ email, team string }{
		{"alice@x.com", "quant"}, {"bob@x.com", "quant"}, {"carol@x.com", ""},
	} {
		if _, err := s.CreateUser(ctx, u.email, "pw-123456"); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		team := u.team
		if _, err := s.SetUserRoleTeam(ctx, u.email, nil, &team); err != nil {
			t.Fatalf("SetUserRoleTeam: %v", err)
		}
	}

	p, err := s.CreateProject(ctx, &Project{
		OwnerEmail: "alice@x.com", Name: "Quant Research", Instructions: "Always cite sources.",
		TeamID: "quant", DefaultPersona: "analyst", DefaultModel: "m-1", MCPServers: []string{"gamma"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Membership: owner + teammate yes; teamless no.
	if !p.MemberOf("alice@x.com", "quant") || !p.MemberOf("bob@x.com", "quant") {
		t.Fatal("owner/teammate must be members")
	}
	if p.MemberOf("carol@x.com", "") {
		t.Fatal("teamless outsider must not be a member")
	}

	// Visibility: bob sees it via team; carol does not.
	bobList, _ := s.ListProjectsForUser(ctx, "bob@x.com", "quant")
	if len(bobList) != 1 || bobList[0].ID != p.ID {
		t.Fatalf("bob visibility: %+v", bobList)
	}
	carolList, _ := s.ListProjectsForUser(ctx, "carol@x.com", "")
	if len(carolList) != 0 {
		t.Fatalf("carol must see nothing: %+v", carolList)
	}

	// Owner-only mutation: bob cannot edit.
	name := "hijack"
	if _, err := s.UpdateProject(ctx, "bob@x.com", p.ID, ProjectPatch{Name: &name}); err == nil {
		t.Fatal("non-owner update must fail")
	}
	instr := "New standing instructions."
	updated, err := s.UpdateProject(ctx, "alice@x.com", p.ID, ProjectPatch{Instructions: &instr, MCPServers: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Instructions != instr || len(updated.MCPServers) != 0 || updated.Name != "Quant Research" {
		t.Fatalf("partial update: %+v", updated)
	}

	// Project conversation binds + inherits (values resolved by the handler).
	conv, err := s.CreateProjectConversation(ctx, "bob@x.com", "chat", "analyst", "m-1", false, p.ID, []string{"gamma"})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, "bob@x.com", conv.ID)
	if got == nil || got.ProjectID != p.ID || len(got.OptionalMCPServersEnabled) != 1 {
		t.Fatalf("project conversation round-trip: %+v", got)
	}

	// Project memory is SHARED and distinct from personal memory.
	pm, err := s.CreateProjectMemory(ctx, p.ID, "bob@x.com", "deploys freeze on Fridays", "constraint")
	if err != nil {
		t.Fatal(err)
	}
	if pm.ProjectID != p.ID {
		t.Fatalf("project memory scope: %+v", pm)
	}
	if _, err := s.CreateMemory(ctx, "bob@x.com", "personal fact", "manual", "fact"); err != nil {
		t.Fatal(err)
	}
	personal, _ := s.ListMemories(ctx, "bob@x.com")
	for _, m := range personal {
		if m.ProjectID != "" {
			t.Fatalf("personal list must exclude project rows: %+v", m)
		}
	}
	shared, _ := s.ListProjectMemories(ctx, p.ID)
	if len(shared) != 1 || shared[0].Content != "deploys freeze on Fridays" {
		t.Fatalf("shared list: %+v", shared)
	}

	// Export references.
	ids, _ := s.ListProjectConversationIDs(ctx, p.ID)
	if len(ids) != 1 || ids[0] != conv.ID {
		t.Fatalf("conversation ids: %v", ids)
	}

	// Delete (owner-only): detaches conversations, removes shared memories.
	if err := s.DeleteProject(ctx, "bob@x.com", p.ID); err == nil {
		t.Fatal("non-owner delete must fail")
	}
	if err := s.DeleteProject(ctx, "alice@x.com", p.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, "bob@x.com", conv.ID)
	if got == nil || got.ProjectID != "" {
		t.Fatalf("delete must detach, not destroy, conversations: %+v", got)
	}
	if after, _ := s.ListProjectMemories(ctx, p.ID); len(after) != 0 {
		t.Fatalf("project memories must die with the project: %+v", after)
	}
}
