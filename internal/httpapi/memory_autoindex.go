package httpapi

import (
	"context"
	"log"
	"strings"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
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
// conversation, and not because it ranked past the 50-item snapshot the model
// was shown. This bounds the memories table's growth to DISTINCT new facts.
//
// Contradiction candidates (#515 stage 2): the extractor sees the user's ACTIVE
// saved memories as a NUMBERED list and may flag that a new fact directly
// contradicts/outdates entry N. The positional claim is resolved to the STABLE
// memory id (+ a content-hash snapshot) HERE, at proposal-creation time — an
// index must never outlive the snapshot it points into. Claims against PINNED
// memories are dropped at claim time (the user protected them). Nothing is
// retired until the human accepts the proposal.
//
// Best-effort: extraction returning nothing, or a single proposal failing, is
// logged and skipped, never fatal. It FAILS SAFE — if the memory set can't be
// loaded it proposes nothing (a missed suggestion beats duplicate-spamming the
// same fact every turn). The caller gates it on cfg.MemoryAutoIndexEnabled and
// skips cancelled/empty turns.
func (s *Server) autoIndexMemories(ctx context.Context, sink agent.EventSink, conversationID, user, userInput, finalText string) {
	// One load serves three needs: the authoritative dedup set, the extractor's
	// numbered known-list, and the supersede-claim id/hash resolution.
	existing, err := s.store.ListMemories(ctx, user)
	if err != nil {
		log.Printf("autoIndexMemories: memory load (user=%s): %v; skipping this turn", user, err)
		return
	}

	// knownActive: the extractor's numbered snapshot — ACTIVE saved memories in
	// the store's stable order (pinned first, newest next), capped like the
	// prompt injection. Only these are supersede-claim targets.
	knownActive := make([]store.Memory, 0, 50)
	knownContents := make([]string, 0, 50)
	for _, m := range existing {
		if m.Source == "proposed" || m.Retired() {
			continue
		}
		if len(knownActive) >= 50 {
			break
		}
		knownActive = append(knownActive, m)
		knownContents = append(knownContents, m.Content)
	}

	facts := s.agent.ExtractMemories(ctx, userInput, finalText, knownContents)
	if len(facts) == 0 {
		return
	}

	seen := make(map[string]bool, len(existing)+len(facts))
	for _, m := range existing {
		seen[normalizeFact(m.Content)] = true
	}

	proposer := &memoryProposer{ctx: ctx, store: s.store, conversationID: conversationID, userEmail: user, sink: sink, origin: "auto"}
	for _, f := range facts {
		key := normalizeFact(f.Content)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		params := store.MemoryProposalParams{Content: f.Content, Kind: f.Kind}
		supersededContent := ""
		if f.Replaces >= 1 && f.Replaces <= len(knownActive) {
			target := knownActive[f.Replaces-1]
			// A pinned memory is user-protected: drop the claim (keep the fact).
			if !target.Pinned {
				params.Supersedes = target.ID
				params.SupersedesHash = store.MemoryContentHash(target.Content)
				supersededContent = target.Content
			}
		}
		if _, err := proposer.propose(params, supersededContent); err != nil {
			log.Printf("autoIndexMemories: propose (user=%s conv=%s): %v", user, conversationID, err)
		}
	}
}

// normalizeFact is the case/whitespace-insensitive key used to dedup an
// extracted fact against existing memories + pending proposals.
func normalizeFact(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
