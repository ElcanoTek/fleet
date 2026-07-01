package httpapi

import (
	"context"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
)

// captureSink records the SSE event names autoIndexMemories emits.
type captureSink struct{ events []string }

func (c *captureSink) Emit(event string, _ any) { c.events = append(c.events, event) }

// TestAutoIndexMemories_DedupAndPropose exercises the #234 auto-indexer against
// the real store: extracted facts that duplicate a known memory (or each other,
// case/space-insensitively) are dropped, and only genuinely-new facts become
// memory PROPOSALS (nothing is written live). Skips without a test DB.
func TestAutoIndexMemories_DedupAndPropose(t *testing.T) {
	s := serverFixture(t)
	ctx := context.Background()
	const user = "alice@x.com"

	conv, err := s.store.CreateConversation(ctx, user, "hi", "victoria", "openrouter/auto", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	// A fact the user already has as a live memory — must NOT be re-proposed.
	if _, err := s.store.CreateMemory(ctx, user, "uses ruff for linting", "chat", ""); err != nil {
		t.Fatalf("CreateMemory: %v", err)
	}

	// The extractor deliberately returns the known fact + a new fact + a
	// case/space variant of that new fact + another new fact, to prove the
	// httpapi-side dedup safety net (independent of the model's own dedup).
	s.agent = &fakeTurnEngine{extractFacts: []agent.ExtractedFact{
		{Content: "uses ruff for linting"},                // dup of a known live memory → skipped
		{Content: "prod db host is db.prod.internal"},     // new → proposed
		{Content: "  Prod DB host is db.prod.internal  "}, // same fact (case/space) → skipped
		{Content: "deploys on fridays"},                   // new → proposed
	}}

	sink := &captureSink{}
	s.autoIndexMemories(ctx, sink, conv.ID, user, "we use ruff", "ok")

	pending, err := s.store.ListPendingMemoryProposalsForConversation(ctx, user, conv.ID)
	if err != nil {
		t.Fatalf("ListPendingMemoryProposalsForConversation: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("want 2 proposals after dedup, got %d: %+v", len(pending), pending)
	}
	got := map[string]bool{}
	for _, p := range pending {
		got[p.Content] = true
	}
	if got["uses ruff for linting"] {
		t.Error("a known live memory must not be re-proposed")
	}
	if !got["prod db host is db.prod.internal"] || !got["deploys on fridays"] {
		t.Errorf("expected the two new facts proposed, got %+v", got)
	}
	if len(sink.events) != 2 {
		t.Errorf("want 2 memory.proposed SSE frames, got %d (%v)", len(sink.events), sink.events)
	}

	// A second identical pass proposes nothing new — the pending proposals are
	// themselves deduped against.
	sink2 := &captureSink{}
	s.autoIndexMemories(ctx, sink2, conv.ID, user, "we use ruff", "ok")
	if len(sink2.events) != 0 {
		t.Errorf("a repeat pass should propose nothing (already pending), emitted %d", len(sink2.events))
	}
	if again, _ := s.store.ListPendingMemoryProposalsForConversation(ctx, user, conv.ID); len(again) != 2 {
		t.Errorf("pending count should stay 2 after a repeat pass, got %d", len(again))
	}
}

// #515: memoryContents must exclude retired + proposed rows and annotate
// non-fact kinds / validity windows for explainability.
func TestMemoryContentsTypedFiltering(t *testing.T) {
	from := int64(1719878400) // 2024-07-02 UTC
	retiredAt := int64(1719878400)
	memories := []store.Memory{
		{Content: "plain fact", Kind: "fact"},
		{Content: "likes short answers", Kind: "preference"},
		{Content: "on-call until Friday", Kind: "context", ValidFrom: &from},
		{Content: "old address", Kind: "fact", RetiredAt: &retiredAt},
		{Content: "undecided", Kind: "fact", Source: "proposed"},
	}
	got := memoryContents(memories)
	want := []string{
		"plain fact",
		"likes short answers (preference)",
		"on-call until Friday (context, true since 2024-07-02)",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("bullet %d: got %q want %q", i, got[i], want[i])
		}
	}
}

// #515 stage 2: a `replaces` claim from the extractor resolves to a STABLE
// memory id + content-hash at proposal time; pinned targets drop the claim;
// accepting the proposal retires the target with retired_by provenance.
func TestAutoIndexMemories_SupersedeCandidate(t *testing.T) {
	s := serverFixture(t)
	ctx := context.Background()
	const user = "bob@x.com"

	conv, err := s.store.CreateConversation(ctx, user, "hi", "victoria", "openrouter/auto", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	stale, err := s.store.CreateMemory(ctx, user, "office is in Boston", "chat", "fact")
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := s.store.CreateMemory(ctx, user, "timezone is EST", "chat", "fact")
	if err != nil {
		t.Fatal(err)
	}
	pin := true
	if _, err := s.store.UpdateMemory(ctx, user, pinned.ID, store.MemoryPatch{Pinned: &pin}); err != nil {
		t.Fatal(err)
	}

	// knownActive ordering is pinned-first then newest: [pinned, stale].
	// Fact 1 contradicts entry 2 (stale); fact 2 claims the PINNED entry 1 —
	// that claim must be dropped at proposal time.
	s.agent = &fakeTurnEngine{extractFacts: []agent.ExtractedFact{
		{Content: "office is in Austin", Kind: "fact", Replaces: 2},
		{Content: "timezone is PST", Kind: "fact", Replaces: 1},
	}}
	sink := &captureSink{}
	s.autoIndexMemories(ctx, sink, conv.ID, user, "we moved", "noted")

	pending, err := s.store.ListPendingMemoryProposalsForConversation(ctx, user, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("want 2 proposals, got %d", len(pending))
	}
	var austin, pst *store.Memory
	for i := range pending {
		switch pending[i].Content {
		case "office is in Austin":
			austin = &pending[i]
		case "timezone is PST":
			pst = &pending[i]
		}
	}
	if austin == nil || austin.Supersedes != stale.ID {
		t.Fatalf("claim must resolve to the stable id: %+v", austin)
	}
	if pst == nil || pst.Supersedes != "" {
		t.Fatalf("claim against a pinned memory must be dropped: %+v", pst)
	}

	// Accepting the claiming proposal retires the stale target.
	accepted, outcome, err := s.store.AcceptMemoryProposal(ctx, user, austin.ID)
	if err != nil || outcome != store.SupersedeRetired {
		t.Fatalf("accept: outcome=%q err=%v", outcome, err)
	}
	all, _ := s.store.ListMemories(ctx, user)
	for _, m := range all {
		if m.ID == stale.ID {
			if !m.Retired() || m.RetiredBy != accepted.ID {
				t.Fatalf("stale memory not retired with provenance: %+v", m)
			}
		}
	}
}
