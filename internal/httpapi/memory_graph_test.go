package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
)

// graphResponse mirrors the GET /memories/graph payload for assertions.
type graphResponse struct {
	Entities  []graphEntityJSON   `json:"entities"`
	Relations []graphRelationJSON `json:"relations"`
}

// seedGraphMemory creates an active memory + a derived fragment directly
// through the store (the handler under test is the READ side).
func seedGraphMemory(t *testing.T, s *Server, user, content string, g store.GraphExtraction) *store.Memory {
	t.Helper()
	m, err := s.store.CreateMemory(context.Background(), user, content, "manual", "fact")
	if err != nil {
		t.Fatalf("CreateMemory: %v", err)
	}
	if _, err := s.store.ReplaceRelationsForMemory(context.Background(), user, m.ID, g); err != nil {
		t.Fatalf("ReplaceRelationsForMemory: %v", err)
	}
	return m
}

func TestMemoryGraphEndpoint(t *testing.T) {
	s := serverFixture(t)
	h := s.Routes()
	const user = "graph@x.com"

	// Empty graph: valid JSON with empty arrays, not null.
	w := do(t, h, http.MethodGet, "/memories/graph", nil, user)
	if w.Code != http.StatusOK {
		t.Fatalf("empty graph: %d %s", w.Code, w.Body.String())
	}
	var empty graphResponse
	if err := json.Unmarshal(w.Body.Bytes(), &empty); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if empty.Entities == nil || empty.Relations == nil || len(empty.Relations) != 0 {
		t.Errorf("empty graph = %s; want empty arrays", w.Body.String())
	}

	long := "Ada works at Elcano Corp. " // >120 runes once repeated → snippet cap
	for len(long) < 200 {
		long += "More context about the arrangement. "
	}
	seedGraphMemory(t, s, user, long, store.GraphExtraction{
		Entities: []store.GraphEntityInput{{Name: "Ada", Type: "person"}, {Name: "Elcano Corp", Type: "organization"}},
		Relations: []store.GraphRelationInput{
			{Subject: "Ada", Predicate: "works at", Object: "Elcano Corp"},
			{Subject: "Ada", Predicate: "prefers", Object: "tabs"},
		},
	})

	w = do(t, h, http.MethodGet, "/memories/graph", nil, user)
	if w.Code != http.StatusOK {
		t.Fatalf("graph: %d %s", w.Code, w.Body.String())
	}
	var resp graphResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Entities) != 2 || len(resp.Relations) != 2 {
		t.Fatalf("graph = %d entities / %d relations, want 2/2: %s", len(resp.Entities), len(resp.Relations), w.Body.String())
	}
	for _, rel := range resp.Relations {
		if rel.Subject.Name != "Ada" || rel.Subject.Type != "person" {
			t.Errorf("subject = %+v; want inlined Ada", rel.Subject)
		}
		if got := []rune(rel.MemoryContentSnippet); len(got) != memorySnippetRunes {
			t.Errorf("snippet runes = %d, want %d", len(got), memorySnippetRunes)
		}
		switch rel.Predicate {
		case "works at":
			if rel.Object.Entity == nil || rel.Object.Entity.Name != "Elcano Corp" || rel.Object.Value != nil {
				t.Errorf("works-at object = %+v; want the entity", rel.Object)
			}
		case "prefers":
			if rel.Object.Value == nil || *rel.Object.Value != "tabs" || rel.Object.Entity != nil {
				t.Errorf("prefers object = %+v; want the literal", rel.Object)
			}
		default:
			t.Errorf("unexpected predicate %q", rel.Predicate)
		}
		if rel.LearnedAt == 0 {
			t.Error("learned_at must be populated from the memory join")
		}
	}

	// Cross-user isolation.
	w = do(t, h, http.MethodGet, "/memories/graph", nil, "other@x.com")
	var other graphResponse
	_ = json.Unmarshal(w.Body.Bytes(), &other)
	if w.Code != http.StatusOK || len(other.Relations) != 0 {
		t.Errorf("other user: %d with %d relations, want 200 empty", w.Code, len(other.Relations))
	}

	// Invalid timestamps → 400; valid RFC3339 as-of params → 200.
	for _, q := range []string{"?as_of_valid=yesterday", "?as_of_learned=1719878400"} {
		if w := do(t, h, http.MethodGet, "/memories/graph"+q, nil, user); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", q, w.Code)
		}
	}
	w = do(t, h, http.MethodGet, "/memories/graph?as_of_valid=2026-01-01T00:00:00Z&as_of_learned=2020-01-01T00:00:00Z", nil, user)
	if w.Code != http.StatusOK {
		t.Fatalf("as-of query: %d %s", w.Code, w.Body.String())
	}
	var past graphResponse
	_ = json.Unmarshal(w.Body.Bytes(), &past)
	if len(past.Relations) != 0 {
		t.Errorf("as-of-learned 2020 should predate the memory; got %d relations", len(past.Relations))
	}

	// Method gate.
	if w := do(t, h, http.MethodPost, "/memories/graph", nil, user); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /memories/graph: %d, want 405", w.Code)
	}
}

func TestMemoriesListAsOf(t *testing.T) {
	s := serverFixture(t)
	h := s.Routes()
	const user = "graph@x.com"
	if _, err := s.store.CreateMemory(context.Background(), user, "a fact", "manual", "fact"); err != nil {
		t.Fatalf("CreateMemory: %v", err)
	}

	w := do(t, h, http.MethodGet, "/memories?as_of_learned=2020-01-01T00:00:00Z", nil, user)
	if w.Code != http.StatusOK {
		t.Fatalf("as-of list: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Memories []store.Memory `json:"memories"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Memories) != 0 {
		t.Errorf("as-of 2020 list should be empty, got %d", len(resp.Memories))
	}

	if w := do(t, h, http.MethodGet, "/memories?as_of_valid=not-a-time", nil, user); w.Code != http.StatusBadRequest {
		t.Errorf("invalid as_of_valid: %d, want 400", w.Code)
	}

	// The plain list is untouched.
	w = do(t, h, http.MethodGet, "/memories", nil, user)
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if w.Code != http.StatusOK || len(resp.Memories) != 1 {
		t.Errorf("plain list: %d with %d memories, want 200/1", w.Code, len(resp.Memories))
	}
}

func TestMemoryExtractGraphEndpoint(t *testing.T) {
	s := serverFixture(t)
	const user = "graph@x.com"
	m, err := s.store.CreateMemory(context.Background(), user, "Ada works at Elcano Corp", "manual", "fact")
	if err != nil {
		t.Fatalf("CreateMemory: %v", err)
	}

	// Flag off (the default) → 503, regardless of the wired extractor.
	s.memoryGraphExtractor = func(context.Context, string) (*agent.ExtractedGraph, error) {
		t.Error("extractor must not run while FLEET_MEMORY_GRAPH_ENABLED is off")
		return nil, nil
	}
	h := s.Routes()
	if w := do(t, h, http.MethodPost, "/memories/"+m.ID+"/extract-graph", nil, user); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("flag off: %d, want 503", w.Code)
	}

	// Flag on + canned extraction → rows land and the count is reported.
	s.cfg.MemoryGraphEnabled = true
	var gotContent string
	s.memoryGraphExtractor = func(_ context.Context, content string) (*agent.ExtractedGraph, error) {
		gotContent = content
		return &agent.ExtractedGraph{
			Entities:  []agent.ExtractedGraphEntity{{Name: "Ada", Type: "person"}},
			Relations: []agent.ExtractedGraphRelation{{Subject: "Ada", Predicate: "works at", Object: "Elcano Corp"}},
		}, nil
	}
	w := do(t, h, http.MethodPost, "/memories/"+m.ID+"/extract-graph", nil, user)
	if w.Code != http.StatusOK {
		t.Fatalf("extract: %d %s", w.Code, w.Body.String())
	}
	if gotContent != m.Content {
		t.Errorf("extractor saw %q, want the memory content", gotContent)
	}
	var res struct {
		MemoryID  string `json:"memory_id"`
		Relations int    `json:"relations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.MemoryID != m.ID || res.Relations != 1 {
		t.Errorf("result = %+v; want 1 relation for %s", res, m.ID)
	}
	g, err := s.store.GraphAsOf(context.Background(), user, store.GraphQuery{})
	if err != nil || len(g.Relations) != 1 {
		t.Fatalf("stored graph = %+v, %v; want the extracted edge", g, err)
	}

	// A failing extractor is surfaced as 502 and stores nothing new.
	s.memoryGraphExtractor = func(context.Context, string) (*agent.ExtractedGraph, error) {
		return nil, errors.New("model unavailable")
	}
	if w := do(t, h, http.MethodPost, "/memories/"+m.ID+"/extract-graph", nil, user); w.Code != http.StatusBadGateway {
		t.Errorf("failing extractor: %d, want 502", w.Code)
	}

	// Unknown id → 404; a pending proposal → 400; foreign user → 404.
	s.memoryGraphExtractor = func(context.Context, string) (*agent.ExtractedGraph, error) { return &agent.ExtractedGraph{}, nil }
	if w := do(t, h, http.MethodPost, "/memories/nope/extract-graph", nil, user); w.Code != http.StatusNotFound {
		t.Errorf("unknown id: %d, want 404", w.Code)
	}
	prop, err := s.store.CreateMemoryProposal(context.Background(), user, "conv-1", store.MemoryProposalParams{Content: "maybe"})
	if err != nil {
		t.Fatalf("CreateMemoryProposal: %v", err)
	}
	if w := do(t, h, http.MethodPost, "/memories/"+prop.ID+"/extract-graph", nil, user); w.Code != http.StatusBadRequest {
		t.Errorf("proposal: %d, want 400", w.Code)
	}
	if w := do(t, h, http.MethodPost, "/memories/"+m.ID+"/extract-graph", nil, "other@x.com"); w.Code != http.StatusNotFound {
		t.Errorf("foreign user: %d, want 404", w.Code)
	}
}

// TestMaybeExtractMemoryGraphGate pins the default-off contract: with the
// flag unset (or no extractor wired) maybeExtractMemoryGraph is a pure no-op,
// so create/accept behavior is byte-for-byte unchanged.
func TestMaybeExtractMemoryGraphGate(t *testing.T) {
	s := serverFixture(t)
	s.memoryGraphExtractor = func(context.Context, string) (*agent.ExtractedGraph, error) {
		t.Error("extractor must not fire while the flag is off")
		return nil, nil
	}
	m, err := s.store.CreateMemory(context.Background(), "graph@x.com", "a fact", "manual", "fact")
	if err != nil {
		t.Fatalf("CreateMemory: %v", err)
	}
	s.maybeExtractMemoryGraph(m) // flag off → no goroutine
	// No extractor wired → no-op even with the flag on.
	s.cfg.MemoryGraphEnabled = true
	s.memoryGraphExtractor = nil
	s.maybeExtractMemoryGraph(m)
	if g, err := s.store.GraphAsOf(context.Background(), "graph@x.com", store.GraphQuery{}); err != nil || len(g.Relations) != 0 {
		t.Errorf("graph = %+v, %v; want empty", g, err)
	}
}
