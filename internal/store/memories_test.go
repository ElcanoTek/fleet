package store

import (
	"context"
	"strings"
	"testing"
)

// TestMemoryProposalConversationScope pins the contract that powers
// re-hydration of pending proposals on conversation load: each proposal
// is associated with the conversation it was made in, AcceptMemoryProposal
// clears the conversation_id (since accepted memories are user-global),
// and ListPendingMemoryProposalsForConversation only returns proposals
// scoped to the requested conversation.
func TestMemoryProposalConversationScope(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	user := "test@example.com"
	convA := "conv-a"
	convB := "conv-b"

	// Two proposals on conv-a, one on conv-b.
	a1, err := s.CreateMemoryProposal(ctx, user, convA, "first thought")
	if err != nil {
		t.Fatalf("create A1: %v", err)
	}
	a2, err := s.CreateMemoryProposal(ctx, user, convA, "second thought")
	if err != nil {
		t.Fatalf("create A2: %v", err)
	}
	b1, err := s.CreateMemoryProposal(ctx, user, convB, "third thought")
	if err != nil {
		t.Fatalf("create B1: %v", err)
	}

	// conv-a list contains exactly A1 + A2, oldest first.
	listA, err := s.ListPendingMemoryProposalsForConversation(ctx, user, convA)
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	if len(listA) != 2 {
		t.Fatalf("conv-a expected 2 proposals, got %d", len(listA))
	}
	// Both proposals must be present. We don't assert their relative order:
	// the query tiebreaks equal created_at by `id ASC` and IDs are random
	// UUIDs, so two proposals created within the same second have no stable
	// order (asserting insertion order here was a ~50% flake).
	gotA := map[string]bool{listA[0].ID: true, listA[1].ID: true}
	if !gotA[a1.ID] || !gotA[a2.ID] {
		t.Errorf("conv-a expected proposals %s and %s; got %s,%s",
			a1.ID, a2.ID, listA[0].ID, listA[1].ID)
	}

	// conv-b is isolated.
	listB, err := s.ListPendingMemoryProposalsForConversation(ctx, user, convB)
	if err != nil {
		t.Fatalf("list B: %v", err)
	}
	if len(listB) != 1 || listB[0].ID != b1.ID {
		t.Fatalf("conv-b expected [%s]; got %d items", b1.ID, len(listB))
	}

	// Accept A1: it should no longer surface as pending for conv-a, AND
	// it should appear in the user's saved memories (source='chat') with
	// conversation_id cleared.
	saved, err := s.AcceptMemoryProposal(ctx, user, a1.ID)
	if err != nil {
		t.Fatalf("accept A1: %v", err)
	}
	if saved.Source != "chat" {
		t.Errorf("accepted source = %q, want 'chat'", saved.Source)
	}

	listA2, err := s.ListPendingMemoryProposalsForConversation(ctx, user, convA)
	if err != nil {
		t.Fatalf("list A after accept: %v", err)
	}
	if len(listA2) != 1 || listA2[0].ID != a2.ID {
		t.Errorf("conv-a after accept expected [%s]; got %d items", a2.ID, len(listA2))
	}

	// The accepted memory shows up in ListMemories (user-global view)
	// but NOT in any conversation's pending list — that's how the UI's
	// header memories control gets it without leaking it into every
	// open chat as a Save/Don't-Save card.
	all, err := s.ListMemories(ctx, user)
	if err != nil {
		t.Fatalf("list all memories: %v", err)
	}
	// Includes a1 (now 'chat') + a2,b1 (still 'proposed'). The 'proposed'
	// rows are filtered out of the system-prompt build elsewhere; here
	// we just verify the row exists.
	hasA1Saved := false
	for _, m := range all {
		if m.ID == a1.ID && m.Source == "chat" {
			hasA1Saved = true
		}
	}
	if !hasA1Saved {
		t.Errorf("ListMemories missing accepted memory %s with source='chat'", a1.ID)
	}

	// Sanity: dismissing a proposal removes it entirely (DELETE path the
	// HTTP handler uses for a Don't-Save click).
	if err := s.DeleteMemory(ctx, user, b1.ID); err != nil {
		t.Fatalf("delete B1: %v", err)
	}
	listB2, err := s.ListPendingMemoryProposalsForConversation(ctx, user, convB)
	if err != nil {
		t.Fatalf("list B after delete: %v", err)
	}
	if len(listB2) != 0 {
		t.Errorf("conv-b after delete expected empty; got %d items", len(listB2))
	}
}

// TestMemoryProposalEmptyContent rejects empty/whitespace content so the
// user never sees a blank Save-this-memory card.
func TestMemoryProposalEmptyContent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, err := s.CreateMemoryProposal(ctx, "test@example.com", "conv-x", "   \n\t  ")
	if err == nil {
		t.Fatal("expected error for whitespace-only content")
	}
	if !strings.Contains(err.Error(), "content required") {
		t.Errorf("error = %v, want 'content required'", err)
	}
}
