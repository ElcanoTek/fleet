package httpapi

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
)

// Temporal knowledge-graph memory (#523): the HTTP seam between the LLM
// extractor (internal/agent) and the derived entity/relation store
// (internal/store). httpapi owns the mapping so store never imports agent —
// the same decoupling posture as runner.ErrorAnalyzer.

// MemoryGraphExtractor mines one memory's content for its graph fragment.
// *agent.Manager's ExtractMemoryGraph satisfies it; cmd/fleet wires it via
// WithMemoryGraphExtractor. nil (the default) disables extraction entirely.
type MemoryGraphExtractor func(ctx context.Context, content string) (*agent.ExtractedGraph, error)

// WithMemoryGraphExtractor wires the LLM knowledge-graph extractor (#523).
// Extraction additionally requires cfg.MemoryGraphEnabled — the option alone
// changes nothing.
func WithMemoryGraphExtractor(fn MemoryGraphExtractor) Option {
	return func(s *Server) { s.memoryGraphExtractor = fn }
}

// memoryGraphExtractBudget bounds one detached extraction goroutine,
// independent of the extractor's internal timeout, so a stuck call can never
// leak a goroutine forever (mirrors runner's errorAnalysisBudget).
const memoryGraphExtractBudget = 60 * time.Second

// memorySnippetRunes is how much memory content the graph API echoes per
// relation — enough to recognize the source fact, not the whole record.
const memorySnippetRunes = 120

// maybeExtractMemoryGraph fires the knowledge-graph extraction off-thread for
// a memory that just became ACTIVE (manual create / accepted proposal). It is
// a no-op unless FLEET_MEMORY_GRAPH_ENABLED is set AND an extractor is wired,
// so default deployments are byte-for-byte unchanged. Mirrors the runner's
// maybeAnalyzeFailure: a detached, time-bounded goroutine whose failures are a
// log line and zero graph rows — never a user-visible error, never a change to
// the memory itself.
func (s *Server) maybeExtractMemoryGraph(m *store.Memory) {
	if s.cfg == nil || !s.cfg.MemoryGraphEnabled || s.memoryGraphExtractor == nil || m == nil {
		return
	}
	// Snapshot primitives before detaching (the caller may reuse m).
	memoryID, user, content := m.ID, m.UserEmail, m.Content
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), memoryGraphExtractBudget)
		defer cancel()
		g, err := s.memoryGraphExtractor(ctx, content)
		if err != nil {
			// %q escapes CR/LF, so a value can't forge a log line.
			log.Printf("memory-graph: extraction for memory %q failed: %v", memoryID, err) //nolint:gosec // G706: memoryID is a store-generated UUID and rendered with %q
			return
		}
		// Detached background context for the write: a validated extraction is
		// worth persisting even if the extraction deadline just elapsed.
		n, err := s.store.ReplaceRelationsForMemory(context.Background(), user, memoryID, toGraphExtraction(g))
		if err != nil {
			log.Printf("memory-graph: persist for memory %q failed: %v", memoryID, err) //nolint:gosec // G706: memoryID is a store-generated UUID and rendered with %q
			return
		}
		log.Printf("memory-graph: memory %q -> %d relation(s)", memoryID, n) //nolint:gosec // G706: memoryID is a store-generated UUID and rendered with %q
	}()
}

// toGraphExtraction maps the agent package's extraction onto the store's
// mirror type (store must not import agent; ADR-0029).
func toGraphExtraction(g *agent.ExtractedGraph) store.GraphExtraction {
	var out store.GraphExtraction
	if g == nil {
		return out
	}
	for _, e := range g.Entities {
		out.Entities = append(out.Entities, store.GraphEntityInput{Name: e.Name, Type: e.Type})
	}
	for _, r := range g.Relations {
		out.Relations = append(out.Relations, store.GraphRelationInput{Subject: r.Subject, Predicate: r.Predicate, Object: r.Object})
	}
	return out
}

// parseAsOfQuery reads the shared as-of query parameters (as_of_valid /
// as_of_learned, RFC3339; project_id) into a store.GraphQuery. ok=false means
// a 400 was already written.
func (s *Server) parseAsOfQuery(w http.ResponseWriter, r *http.Request) (store.GraphQuery, bool) {
	var q store.GraphQuery
	for param, dst := range map[string]**time.Time{
		"as_of_valid":   &q.AsOfValid,
		"as_of_learned": &q.AsOfLearned,
	} {
		raw := r.URL.Query().Get(param)
		if raw == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			http.Error(w, param+" must be an RFC3339 timestamp", http.StatusBadRequest)
			return q, false
		}
		*dst = &t
	}
	if pid := r.URL.Query().Get("project_id"); pid != "" {
		q.ProjectID = &pid
	}
	return q, true
}

// graphEntityJSON / graphRelationJSON shape the GET /memories/graph response.
type graphEntityJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// graphObjectJSON carries exactly one of Entity (an entity→entity edge) or
// Value (a literal attribute) — mirroring the DB CHECK constraint.
type graphObjectJSON struct {
	Entity *graphEntityJSON `json:"entity,omitempty"`
	Value  *string          `json:"value,omitempty"`
}

type graphRelationJSON struct {
	ID                   string          `json:"id"`
	Subject              graphEntityJSON `json:"subject"`
	Predicate            string          `json:"predicate"`
	Object               graphObjectJSON `json:"object"`
	MemoryID             string          `json:"memory_id"`
	MemoryContentSnippet string          `json:"memory_content_snippet"`
	LearnedAt            int64           `json:"learned_at"`
	ValidFrom            *int64          `json:"valid_from,omitempty"`
	ValidTo              *int64          `json:"valid_to,omitempty"`
}

// memoryGraph serves GET /memories/graph (#523): the user's derived knowledge
// graph as of the queried coordinates (see store.GraphQuery for the two-axis
// semantics). Always readable — the graph may simply be empty when extraction
// is off. project_id is membership-gated like every other project read.
func (s *Server) memoryGraph(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user := userFromCtx(r.Context())
	q, ok := s.parseAsOfQuery(w, r)
	if !ok {
		return
	}
	if q.ProjectID != nil {
		if p := s.projectForMember(w, r, user, *q.ProjectID); p == nil {
			return
		}
	}
	g, err := s.store.GraphAsOf(r.Context(), user, q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	entitiesByID := make(map[string]graphEntityJSON, len(g.Entities))
	entities := make([]graphEntityJSON, 0, len(g.Entities))
	for _, e := range g.Entities {
		ej := graphEntityJSON{ID: e.ID, Name: e.Name, Type: e.Type}
		entitiesByID[e.ID] = ej
		entities = append(entities, ej)
	}
	relations := make([]graphRelationJSON, 0, len(g.Relations))
	for _, rel := range g.Relations {
		rj := graphRelationJSON{
			ID:                   rel.ID,
			Subject:              entitiesByID[rel.SubjectEntityID],
			Predicate:            rel.Predicate,
			MemoryID:             rel.MemoryID,
			MemoryContentSnippet: snippet(rel.MemoryContent, memorySnippetRunes),
			LearnedAt:            rel.LearnedAt,
			ValidFrom:            rel.ValidFrom,
			ValidTo:              rel.ValidTo,
		}
		if rel.ObjectEntityID != "" {
			obj := entitiesByID[rel.ObjectEntityID]
			rj.Object.Entity = &obj
		} else {
			v := rel.ObjectValue
			rj.Object.Value = &v
		}
		relations = append(relations, rj)
	}
	writeJSON(w, map[string]any{"entities": entities, "relations": relations})
}

// handleMemoryExtractGraph serves POST /memories/{id}/extract-graph (#523):
// manual (re-)extraction of one memory's graph fragment, synchronous so the
// caller sees the result. It requires the same FLEET_MEMORY_GRAPH_ENABLED
// gate as the async path (503 when off, matching the remote-MCP "not
// configured" posture) and refuses proposals — an unreviewed candidate is not
// knowledge.
func (s *Server) handleMemoryExtractGraph(w http.ResponseWriter, r *http.Request, user, id string) {
	if s.cfg == nil || !s.cfg.MemoryGraphEnabled || s.memoryGraphExtractor == nil {
		http.Error(w, "memory graph extraction is not enabled on this server (set FLEET_MEMORY_GRAPH_ENABLED=true)", http.StatusServiceUnavailable)
		return
	}
	m, err := s.store.GetMemory(r.Context(), user, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if m.Source == "proposed" {
		http.Error(w, "cannot extract a graph from a pending proposal", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), memoryGraphExtractBudget)
	defer cancel()
	g, err := s.memoryGraphExtractor(ctx, m.Content)
	if err != nil {
		log.Printf("memory-graph: manual extraction for memory %q failed: %v", id, err) //nolint:gosec // G706: %q escapes CR/LF, so the request-supplied id cannot forge a log line
		http.Error(w, "graph extraction failed", http.StatusBadGateway)
		return
	}
	n, err := s.store.ReplaceRelationsForMemory(r.Context(), user, id, toGraphExtraction(g))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"memory_id": id, "relations": n})
}

// snippet returns the first n runes of s (the graph API's per-relation echo
// of its source memory).
func snippet(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
