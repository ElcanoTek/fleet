// Personal memory endpoints: GET/POST /memories and the /memories/{id} item
// routes (accept / extract-graph / PATCH / DELETE). Split out of server.go
// (#1127). The knowledge-graph read side lives in memory_graph.go.

package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/ElcanoTek/fleet/internal/store"
)

// ── /memories ─────────────────────────────────────────────────────────────

type memoryRequest struct {
	Content string `json:"content"`
	Kind    string `json:"kind"`
}

// memoryPatchRequest is the PATCH /memories/{id} body: nil fields untouched
// (#515). valid_from/valid_to use 0 to clear the bound; retired=true is manual
// retirement (kept for audit, excluded from injection), false restores.
type memoryPatchRequest struct {
	Content   *string `json:"content"`
	Kind      *string `json:"kind"`
	Pinned    *bool   `json:"pinned"`
	Retired   *bool   `json:"retired"`
	ValidFrom *int64  `json:"valid_from"`
	ValidTo   *int64  `json:"valid_to"`
}

func (s *Server) memories(w http.ResponseWriter, r *http.Request) {
	user := userFromCtx(r.Context())
	switch r.Method {
	case http.MethodGet:
		// As-of time travel (#523): with either as_of param the list answers
		// "what was true / what did fleet know at that instant" instead of
		// "everything, retired rows trailing" (see store.GraphQuery).
		if r.URL.Query().Get("as_of_valid") != "" || r.URL.Query().Get("as_of_learned") != "" {
			q, ok := s.parseAsOfQuery(w, r)
			if !ok {
				return
			}
			memories, err := s.store.ListMemoriesAsOf(r.Context(), user, q)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]any{"memories": memories})
			return
		}
		memories, err := s.store.ListMemories(r.Context(), user)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"memories": memories})
	case http.MethodPost:
		var req memoryRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		memory, err := s.store.CreateMemory(r.Context(), user, req.Content, "manual", req.Kind)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// A manually created memory is ACTIVE immediately → derive its graph
		// fragment (async, best-effort, gated; #523).
		s.maybeExtractMemoryGraph(memory)
		writeJSON(w, memory)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) memoryByID(w http.ResponseWriter, r *http.Request) {
	user := userFromCtx(r.Context())
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/memories/"), "/")
	if rest == "" {
		http.Error(w, "memory id required", http.StatusBadRequest)
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	sub := ""
	if len(parts) == 2 {
		sub = parts[1]
	}
	if sub == "accept" && r.Method == http.MethodPost {
		// Destination (Item D1): an optional {"project_id": ...} accepts the
		// proposal into that project's TEAM LEARNINGS instead of the caller's
		// personal memory — the choice the approval card now shows before the
		// user saves, rather than defaulting silently. Membership is
		// re-checked here; the body is optional, so a client that never sends
		// one keeps the personal behavior byte for byte.
		var dest struct {
			ProjectID string `json:"project_id"`
		}
		if r.Body != nil {
			// A MALFORMED body is refused, not ignored. Swallowing the error
			// fell through to the personal-memory accept and answered 200 —
			// so a client that meant "save this to the team" was told it
			// succeeded while the memory went somewhere else. io.EOF is the
			// legitimate empty body (the personal path) and is not an error.
			if derr := json.NewDecoder(r.Body).Decode(&dest); derr != nil && !errors.Is(derr, io.EOF) {
				http.Error(w, "bad json: "+derr.Error(), http.StatusBadRequest)
				return
			}
		}
		if projectID := strings.TrimSpace(dest.ProjectID); projectID != "" {
			p := s.projectForMember(w, r, user, projectID)
			if p == nil {
				return
			}
			memory, err := s.store.AcceptMemoryProposalIntoProject(r.Context(), user, id, p.ID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			// No graph extraction: the knowledge graph is a personal-memory
			// derivation (#523) and a project row is not part of it.
			writeJSON(w, map[string]any{"memory": memory, "supersede": store.SupersedeNone, "project_id": p.ID})
			return
		}
		memory, supersede, err := s.store.AcceptMemoryProposal(r.Context(), user, id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Acceptance made the memory ACTIVE → derive its graph fragment
		// (async, best-effort, gated; #523).
		s.maybeExtractMemoryGraph(memory)
		// The envelope (vs the bare memory) carries the #515 stage-2 outcome:
		// what happened to the older fact this proposal claimed to replace
		// ("retired", or the guard that kept it — pinned/changed/missing/…).
		writeJSON(w, map[string]any{"memory": memory, "supersede": supersede})
		return
	}
	if sub == "extract-graph" && r.Method == http.MethodPost {
		s.handleMemoryExtractGraph(w, r, user, id)
		return
	}
	switch r.Method {
	case http.MethodPatch:
		var req memoryPatchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		memory, err := s.store.UpdateMemory(r.Context(), user, id, store.MemoryPatch{
			Content:   req.Content,
			Kind:      req.Kind,
			Pinned:    req.Pinned,
			Retired:   req.Retired,
			ValidFrom: req.ValidFrom,
			ValidTo:   req.ValidTo,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, memory)
	case http.MethodDelete:
		if err := s.store.DeleteMemory(r.Context(), user, id); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
