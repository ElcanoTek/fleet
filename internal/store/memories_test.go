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
	a1, err := s.CreateMemoryProposal(ctx, user, convA, MemoryProposalParams{Content: "first thought", Origin: "tool"})
	if err != nil {
		t.Fatalf("create A1: %v", err)
	}
	a2, err := s.CreateMemoryProposal(ctx, user, convA, MemoryProposalParams{Content: "second thought", Origin: "tool"})
	if err != nil {
		t.Fatalf("create A2: %v", err)
	}
	b1, err := s.CreateMemoryProposal(ctx, user, convB, MemoryProposalParams{Content: "third thought", Origin: "tool"})
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
	saved, _, err := s.AcceptMemoryProposal(ctx, user, a1.ID)
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

	_, err := s.CreateMemoryProposal(ctx, "test@example.com", "conv-x", MemoryProposalParams{Content: "   \n\t  ", Origin: "tool"})
	if err == nil {
		t.Fatal("expected error for whitespace-only content")
	}
	if !strings.Contains(err.Error(), "content required") {
		t.Errorf("error = %v, want 'content required'", err)
	}
}

// #515 typed memory MVP: kind normalization, partial patch, retirement,
// validity window, pinned-first ordering, and provenance retention on accept.
func TestTypedMemoryLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	user := "typed@example.com"

	// Unknown kind normalizes to "fact"; known kinds stick.
	m, err := s.CreateMemory(ctx, user, "likes terse answers", "manual", "preference")
	if err != nil {
		t.Fatal(err)
	}
	if m.Kind != "preference" || m.Origin != "manual" || m.LearnedAt == 0 {
		t.Fatalf("typed create: %+v", m)
	}
	weird, err := s.CreateMemory(ctx, user, "weird kind", "manual", "vibe")
	if err != nil {
		t.Fatal(err)
	}
	if weird.Kind != "fact" {
		t.Fatalf("unknown kind must normalize to fact, got %q", weird.Kind)
	}

	// Partial patch: pin + validity window; content untouched.
	from := int64(1700000000)
	pinTrue := true
	patched, err := s.UpdateMemory(ctx, user, m.ID, MemoryPatch{Pinned: &pinTrue, ValidFrom: &from})
	if err != nil {
		t.Fatal(err)
	}
	if !patched.Pinned || patched.ValidFrom == nil || *patched.ValidFrom != from || patched.Content != "likes terse answers" {
		t.Fatalf("patch: %+v", patched)
	}
	// 0 clears a validity bound.
	zero := int64(0)
	cleared, err := s.UpdateMemory(ctx, user, m.ID, MemoryPatch{ValidFrom: &zero})
	if err != nil {
		t.Fatal(err)
	}
	if cleared.ValidFrom != nil {
		t.Fatalf("valid_from must clear on 0, got %v", *cleared.ValidFrom)
	}
	// Empty patch is an error.
	if _, err := s.UpdateMemory(ctx, user, m.ID, MemoryPatch{}); err == nil {
		t.Fatal("empty patch must error")
	}

	// Manual retirement + restore.
	retire := true
	retired, err := s.UpdateMemory(ctx, user, weird.ID, MemoryPatch{Retired: &retire})
	if err != nil {
		t.Fatal(err)
	}
	if !retired.Retired() || retired.RetiredBy != "" {
		t.Fatalf("manual retire: %+v", retired)
	}
	// Retired rows list AFTER active ones; pinned actives first.
	list, err := s.ListMemories(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != m.ID || !list[0].Pinned || !list[1].Retired() {
		t.Fatalf("ordering: %+v", list)
	}
	restore := false
	restored, err := s.UpdateMemory(ctx, user, weird.ID, MemoryPatch{Retired: &restore})
	if err != nil {
		t.Fatal(err)
	}
	if restored.Retired() {
		t.Fatalf("restore failed: %+v", restored)
	}
}

func TestAcceptMemoryProposalRetainsProvenance(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	user := "prov@example.com"
	p, err := s.CreateMemoryProposal(ctx, user, "conv-42", MemoryProposalParams{Content: "team uses trunk-based dev", Kind: "constraint", Origin: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != "constraint" || p.Origin != "auto" || p.ConversationID != "conv-42" {
		t.Fatalf("proposal provenance: %+v", p)
	}
	got, _, err := s.AcceptMemoryProposal(ctx, user, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	// #515: conversation_id is provenance and must SURVIVE acceptance.
	if got.Source != "chat" || got.ConversationID != "conv-42" || got.Kind != "constraint" || got.Origin != "auto" {
		t.Fatalf("accept must retain provenance: %+v", got)
	}
	// Retained conversation_id must NOT re-mark the row as pending.
	pending, err := s.ListPendingMemoryProposalsForConversation(ctx, user, "conv-42")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("accepted row still pending: %+v", pending)
	}
}

// #515 stage 2: supersede-on-accept — the accepted proposal retires its
// claimed target only when every guard holds; every guard failure is an
// OUTCOME (the accept still succeeds), never a silent retire.
func TestAcceptMemoryProposalSupersede(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	user := "supersede@example.com"

	mkTarget := func(content string) *Memory {
		t.Helper()
		m, err := s.CreateMemory(ctx, user, content, "chat", "fact")
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	mkProposal := func(content string, target *Memory, hash string) *Memory {
		t.Helper()
		p, err := s.CreateMemoryProposal(ctx, user, "conv-1", MemoryProposalParams{
			Content: content, Kind: "fact", Origin: "auto",
			Supersedes: target.ID, SupersedesHash: hash,
		})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	// Happy path: target retired, retired_by links the accepted memory.
	target := mkTarget("office is in Boston")
	prop := mkProposal("office is in Austin", target, MemoryContentHash(target.Content))
	accepted, outcome, err := s.AcceptMemoryProposal(ctx, user, prop.ID)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != SupersedeRetired || accepted.Source != "chat" {
		t.Fatalf("outcome=%q accepted=%+v", outcome, accepted)
	}
	list, _ := s.ListMemories(ctx, user)
	var reloaded *Memory
	for i := range list {
		if list[i].ID == target.ID {
			reloaded = &list[i]
		}
	}
	if reloaded == nil || !reloaded.Retired() || reloaded.RetiredBy != accepted.ID {
		t.Fatalf("target retirement: %+v", reloaded)
	}

	// Pinned target: kept, outcome reported.
	pinnedTarget := mkTarget("timezone is EST")
	pin := true
	if _, err := s.UpdateMemory(ctx, user, pinnedTarget.ID, MemoryPatch{Pinned: &pin}); err != nil {
		t.Fatal(err)
	}
	prop2 := mkProposal("timezone is PST", pinnedTarget, MemoryContentHash(pinnedTarget.Content))
	_, outcome, err = s.AcceptMemoryProposal(ctx, user, prop2.ID)
	if err != nil || outcome != SupersedeTargetPinned {
		t.Fatalf("pinned guard: outcome=%q err=%v", outcome, err)
	}

	// Target edited after the claim: hash mismatch → kept.
	edited := mkTarget("editor is vim")
	prop3 := mkProposal("editor is emacs", edited, MemoryContentHash(edited.Content))
	newContent := "editor is neovim"
	if _, err := s.UpdateMemory(ctx, user, edited.ID, MemoryPatch{Content: &newContent}); err != nil {
		t.Fatal(err)
	}
	_, outcome, err = s.AcceptMemoryProposal(ctx, user, prop3.ID)
	if err != nil || outcome != SupersedeTargetChanged {
		t.Fatalf("hash guard: outcome=%q err=%v", outcome, err)
	}

	// Two proposals against the same target: the second accept reports
	// target_retired instead of double-retiring.
	shared := mkTarget("runs postgres 15")
	pA := mkProposal("runs postgres 16", shared, MemoryContentHash(shared.Content))
	pB := mkProposal("runs postgres 17", shared, MemoryContentHash(shared.Content))
	_, o1, err := s.AcceptMemoryProposal(ctx, user, pA.ID)
	if err != nil || o1 != SupersedeRetired {
		t.Fatalf("first accept: %q %v", o1, err)
	}
	_, o2, err := s.AcceptMemoryProposal(ctx, user, pB.ID)
	if err != nil || o2 != SupersedeTargetRetired {
		t.Fatalf("second accept must not double-retire: %q %v", o2, err)
	}

	// Deleted target: outcome target_missing, accept still succeeds.
	gone := mkTarget("uses jira")
	prop4 := mkProposal("uses linear", gone, MemoryContentHash(gone.Content))
	if err := s.DeleteMemory(ctx, user, gone.ID); err != nil {
		t.Fatal(err)
	}
	acc4, o4, err := s.AcceptMemoryProposal(ctx, user, prop4.ID)
	if err != nil || o4 != SupersedeTargetMissing || acc4.Source != "chat" {
		t.Fatalf("missing guard: %q %v %+v", o4, err, acc4)
	}
}
