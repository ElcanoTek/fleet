package store

import (
	"context"
	"testing"
)

// Pinning a project is a normal owner-scoped patch: the flag round-trips,
// and a non-owner's attempt is rejected by the owner-scoped UPDATE.
func TestProjectPin_PatchAndOwnerScope(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	pinned := true

	proj, err := s.CreateProject(ctx, &Project{OwnerEmail: "u@x.com", Name: "Ops"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if proj.Pinned {
		t.Error("a new project must start unpinned")
	}

	updated, err := s.UpdateProject(ctx, "u@x.com", proj.ID, ProjectPatch{Pinned: &pinned})
	if err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	if !updated.Pinned {
		t.Error("pin did not stick on the returned project")
	}
	got, err := s.GetProject(ctx, proj.ID)
	if err != nil || got == nil || !got.Pinned {
		t.Errorf("pin did not persist: %+v (%v)", got, err)
	}

	if _, err := s.UpdateProject(ctx, "other@x.com", proj.ID, ProjectPatch{Pinned: &pinned}); err == nil {
		t.Error("non-owner pin patch must fail")
	}
}

// Filing a chat into a project clears its pin (mirroring SetArchived): the
// chat lives only under the project, and a stale pinned flag would make it
// pop back into Pinned on a later unfile. Red/green: without the CASE in
// SetConversationProject the flag survives filing.
func TestSetConversationProject_UnpinsOnFile(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const user = "u@x.com"

	proj, err := s.CreateProject(ctx, &Project{OwnerEmail: user, Name: "Keep"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	c, err := s.CreateConversation(ctx, user, "t", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if err := s.SetPinned(ctx, user, c.ID, true); err != nil {
		t.Fatalf("SetPinned: %v", err)
	}

	if err := s.SetConversationProject(ctx, user, c.ID, proj.ID); err != nil {
		t.Fatalf("SetConversationProject: %v", err)
	}
	got, err := s.Get(ctx, user, c.ID)
	if err != nil || got == nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Pinned {
		t.Error("filing into a project must clear the pin")
	}

	// Unfiling must NOT resurrect the pin.
	if err := s.SetConversationProject(ctx, user, c.ID, ""); err != nil {
		t.Fatalf("unfile: %v", err)
	}
	got, err = s.Get(ctx, user, c.ID)
	if err != nil || got == nil {
		t.Fatalf("Get after unfile: %v", err)
	}
	if got.Pinned {
		t.Error("unfiling must leave the chat unpinned")
	}
}
