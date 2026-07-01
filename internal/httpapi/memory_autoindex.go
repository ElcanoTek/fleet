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
// It dedups against the user's ENTIRE memory set — live (chat/manual) AND
// still-pending 'proposed' rows, across ALL conversations — so a fact is
// proposed once and never again: not after it is saved, not in another
// conversation, and not because it ranked past the 50-item prompt snapshot the
// model was shown. This bounds the memories table's growth to DISTINCT new
// facts. `known` (the capped prompt snapshot) is only the model's do-not-repeat
// hint; the authoritative dedup below uses the full set.
//
// Best-effort: extraction returning nothing, or a single proposal failing, is
// logged and skipped, never fatal. It FAILS SAFE — if the dedup set can't be
// loaded it proposes nothing (a missed suggestion beats duplicate-spamming the
// same fact every turn). The caller gates it on cfg.MemoryAutoIndexEnabled and
// skips cancelled/empty turns.
func (s *Server) autoIndexMemories(ctx context.Context, sink agent.EventSink, conversationID, user, userInput, finalText string, known []string) {
	facts := s.agent.ExtractMemories(ctx, userInput, finalText, known)
	if len(facts) == 0 {
		return
	}

	// ListMemories applies no source filter, so it returns 'proposed' rows too —
	// giving one dedup set covering saved memories AND undecided proposals in
	// every conversation.
	existing, err := s.store.ListMemories(ctx, user)
	if err != nil {
		log.Printf("autoIndexMemories: dedup load (user=%s): %v; skipping this turn", user, err)
		return
	}
	seen := make(map[string]bool, len(existing)+len(facts))
	for _, m := range existing {
		seen[normalizeFact(m.Content)] = true
	}

	proposer := &memoryProposer{ctx: ctx, store: s.store, conversationID: conversationID, userEmail: user, sink: sink, origin: "auto"}
	for _, f := range facts {
		key := normalizeFact(f)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		if _, err := proposer.Propose(f, ""); err != nil {
			log.Printf("autoIndexMemories: propose (user=%s conv=%s): %v", user, conversationID, err)
		}
	}
}

// normalizeFact is the case/whitespace-insensitive key used to dedup an
// extracted fact against existing memories + pending proposals.
func normalizeFact(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
