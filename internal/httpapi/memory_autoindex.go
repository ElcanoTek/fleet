package httpapi

import (
	"context"
	"log"
	"strings"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// autoIndexMemories mines a completed turn for durable facts (#234) and surfaces
// each NEW one as a memory PROPOSAL through the SAME seam the propose_memory tool
// uses (memoryProposer: CreateMemoryProposal + a memory.proposed SSE frame the
// chat UI renders as a Save/Don't-Save card). It writes nothing live — the user
// still confirms every fact — so it respects the human-on-the-loop memory model.
//
// It dedups against the user's known live memories AND this conversation's
// already-pending proposals, so a fact stated across several turns is proposed
// once, not on every turn. Best-effort: extraction returning nothing, or a
// single proposal failing, is logged and skipped, never fatal. The caller gates
// it on cfg.MemoryAutoIndexEnabled and skips cancelled/empty turns.
func (s *Server) autoIndexMemories(ctx context.Context, sink agent.EventSink, conversationID, user, userInput, finalText string, known []string) {
	facts := s.agent.ExtractMemories(ctx, userInput, finalText, known)
	if len(facts) == 0 {
		return
	}

	seen := make(map[string]bool, len(known)+len(facts))
	for _, k := range known {
		seen[normalizeFact(k)] = true
	}
	// A fact already staged (source='proposed') for this conversation must not be
	// re-proposed — the earlier card is still pending the user's decision.
	if pending, err := s.store.ListPendingMemoryProposalsForConversation(ctx, user, conversationID); err == nil {
		for _, p := range pending {
			seen[normalizeFact(p.Content)] = true
		}
	}

	proposer := &memoryProposer{ctx: ctx, store: s.store, conversationID: conversationID, userEmail: user, sink: sink}
	for _, f := range facts {
		key := normalizeFact(f)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		if _, err := proposer.Propose(f); err != nil {
			log.Printf("autoIndexMemories: propose (user=%s conv=%s): %v", user, conversationID, err)
		}
	}
}

// normalizeFact is the case/whitespace-insensitive key used to dedup an
// extracted fact against existing memories + pending proposals.
func normalizeFact(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
