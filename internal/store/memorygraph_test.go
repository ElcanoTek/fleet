package store

import (
	"context"
	"testing"
	"time"
)

// backdateMemory rewrites a memory's transaction-time bookkeeping directly —
// CreateMemory always stamps learned_at=now, and the time-travel tests need
// history to travel over.
func backdateMemory(t *testing.T, s *Store, id string, learnedAt int64, retiredAt *int64) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`UPDATE memories SET learned_at = $1, retired_at = $2 WHERE id = $3`,
		learnedAt, retiredAt, id); err != nil {
		t.Fatalf("backdate %s: %v", id, err)
	}
}

// graphTestUser is the fixture user every graph test writes as; cross-user
// isolation cases use explicit other emails inline.
const graphTestUser = "graph@x.com"

func mustCreateMemory(t *testing.T, s *Store, content string) *Memory {
	t.Helper()
	m, err := s.CreateMemory(context.Background(), graphTestUser, content, "manual", "fact")
	if err != nil {
		t.Fatalf("CreateMemory(%q): %v", content, err)
	}
	return m
}

func mustReplaceRelations(t *testing.T, s *Store, memoryID string, g GraphExtraction) int {
	t.Helper()
	n, err := s.ReplaceRelationsForMemory(context.Background(), graphTestUser, memoryID, g)
	if err != nil {
		t.Fatalf("ReplaceRelationsForMemory: %v", err)
	}
	return n
}

func TestUpsertEntityNormalization(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const user = "graph@x.com"

	a, err := s.UpsertEntity(ctx, user, "  Elcano   Corp ", "organization")
	if err != nil {
		t.Fatalf("UpsertEntity: %v", err)
	}
	if a.Name != "Elcano Corp" || a.Type != "organization" {
		t.Errorf("entity = %+v; want collapsed name + declared type", a)
	}

	// Case/space variant of the same (name, type) → the SAME node, display
	// name refreshed to the newest extraction's casing.
	b, err := s.UpsertEntity(ctx, user, "elcano corp", "organization")
	if err != nil {
		t.Fatalf("UpsertEntity dup: %v", err)
	}
	if b.ID != a.ID {
		t.Errorf("case/space variant created a second node: %s vs %s", b.ID, a.ID)
	}
	if b.Name != "elcano corp" {
		t.Errorf("display name should follow the freshest upsert, got %q", b.Name)
	}

	// Same name, different type → a DIFFERENT node (the type is part of the key).
	c, err := s.UpsertEntity(ctx, user, "Elcano Corp", "project")
	if err != nil {
		t.Fatalf("UpsertEntity other type: %v", err)
	}
	if c.ID == a.ID {
		t.Error("same name with a different type must be a distinct node")
	}

	// Unknown type normalizes to "other"; blank name is an error.
	d, err := s.UpsertEntity(ctx, user, "Ada", "wizard")
	if err != nil {
		t.Fatalf("UpsertEntity unknown type: %v", err)
	}
	if d.Type != "other" {
		t.Errorf("unknown type = %q; want other", d.Type)
	}
	if _, err := s.UpsertEntity(ctx, user, "   ", "person"); err == nil {
		t.Error("blank entity name should error")
	}
}

func TestReplaceRelationsForMemory(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const user = "graph@x.com"
	m := mustCreateMemory(t, s, "Ada works at Elcano Corp and prefers tabs")

	extraction := GraphExtraction{
		Entities: []GraphEntityInput{
			{Name: "Ada", Type: "person"},
			{Name: "Elcano Corp", Type: "organization"},
		},
		Relations: []GraphRelationInput{
			{Subject: "Ada", Predicate: "Works At", Object: "Elcano Corp"}, // entity object
			{Subject: "Ada", Predicate: "prefers", Object: "tabs"},         // literal object
			{Subject: "ada", Predicate: "works at", Object: "elcano corp"}, // dup triple → dropped
			{Subject: "", Predicate: "x", Object: "y"},                     // blank subject → dropped
		},
	}
	if n := mustReplaceRelations(t, s, m.ID, extraction); n != 2 {
		t.Fatalf("inserted %d relations, want 2", n)
	}

	g, err := s.GraphAsOf(ctx, user, GraphQuery{})
	if err != nil {
		t.Fatalf("GraphAsOf: %v", err)
	}
	if len(g.Relations) != 2 {
		t.Fatalf("relations = %d, want 2: %+v", len(g.Relations), g.Relations)
	}
	if len(g.Entities) != 2 {
		t.Fatalf("entities = %d, want 2 (only referenced nodes): %+v", len(g.Entities), g.Entities)
	}
	byPred := map[string]GraphRelation{}
	for _, r := range g.Relations {
		byPred[r.Predicate] = r
		if r.MemoryID != m.ID || r.MemoryContent != m.Content {
			t.Errorf("relation provenance = %q/%q; want the source memory", r.MemoryID, r.MemoryContent)
		}
		if r.LearnedAt != m.LearnedAt {
			t.Errorf("relation learned_at = %d; want the MEMORY's %d (relations own no time)", r.LearnedAt, m.LearnedAt)
		}
	}
	works, ok := byPred["works at"] // predicate normalized to lower
	if !ok || works.ObjectEntityID == "" || works.ObjectValue != "" {
		t.Errorf("works-at should be an entity→entity edge, got %+v", works)
	}
	prefers, ok := byPred["prefers"]
	if !ok || prefers.ObjectEntityID != "" || prefers.ObjectValue != "tabs" {
		t.Errorf("prefers should be a literal edge with value 'tabs', got %+v", prefers)
	}

	// Re-extraction is idempotent: a second identical pass replaces (not
	// duplicates) the fragment; a corrected pass fully swaps it.
	if n := mustReplaceRelations(t, s, m.ID, extraction); n != 2 {
		t.Fatalf("re-extract inserted %d, want 2", n)
	}
	g2, _ := s.GraphAsOf(ctx, user, GraphQuery{})
	if len(g2.Relations) != 2 {
		t.Fatalf("after re-extract relations = %d, want 2 (no duplicates)", len(g2.Relations))
	}
	corrected := GraphExtraction{
		Entities:  []GraphEntityInput{{Name: "Ada", Type: "person"}},
		Relations: []GraphRelationInput{{Subject: "Ada", Predicate: "prefers", Object: "spaces"}},
	}
	if n := mustReplaceRelations(t, s, m.ID, corrected); n != 1 {
		t.Fatalf("corrected extract inserted %d, want 1", n)
	}
	g3, _ := s.GraphAsOf(ctx, user, GraphQuery{})
	if len(g3.Relations) != 1 || g3.Relations[0].ObjectValue != "spaces" {
		t.Fatalf("corrected graph = %+v; want the single new edge", g3.Relations)
	}

	// An undeclared subject is kept, implicitly typed "other".
	implicit := GraphExtraction{
		Relations: []GraphRelationInput{{Subject: "Mystery Tool", Predicate: "runs on", Object: "port 8080"}},
	}
	if n := mustReplaceRelations(t, s, m.ID, implicit); n != 1 {
		t.Fatalf("implicit-subject extract inserted %d, want 1", n)
	}
	g4, _ := s.GraphAsOf(ctx, user, GraphQuery{})
	if len(g4.Entities) != 1 || g4.Entities[0].Name != "Mystery Tool" || g4.Entities[0].Type != "other" {
		t.Fatalf("implicit subject entities = %+v; want one 'other'-typed node", g4.Entities)
	}

	// Scoping: another user's memory id is "not found"; so is a bogus id.
	if _, err := s.ReplaceRelationsForMemory(ctx, "someone-else@x.com", m.ID, extraction); err == nil {
		t.Error("foreign user must not write another user's graph")
	}
	if _, err := s.ReplaceRelationsForMemory(ctx, user, "nope", extraction); err == nil {
		t.Error("unknown memory id should error")
	}
}

func TestMemoryDeleteCascadesRelations(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const user = "graph@x.com"
	m := mustCreateMemory(t, s, "Ada works at Elcano Corp")
	mustReplaceRelations(t, s, m.ID, GraphExtraction{
		Entities:  []GraphEntityInput{{Name: "Ada", Type: "person"}, {Name: "Elcano Corp", Type: "organization"}},
		Relations: []GraphRelationInput{{Subject: "Ada", Predicate: "works at", Object: "Elcano Corp"}},
	})
	if err := s.DeleteMemory(ctx, user, m.ID); err != nil {
		t.Fatalf("DeleteMemory: %v", err)
	}
	var relations int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_relations`).Scan(&relations); err != nil {
		t.Fatalf("count relations: %v", err)
	}
	if relations != 0 {
		t.Errorf("relations after memory delete = %d, want 0 (ON DELETE CASCADE)", relations)
	}
	// Entities survive (shared nodes) but the graph view no longer shows them.
	g, err := s.GraphAsOf(ctx, user, GraphQuery{})
	if err != nil {
		t.Fatalf("GraphAsOf: %v", err)
	}
	if len(g.Relations) != 0 || len(g.Entities) != 0 {
		t.Errorf("graph after delete = %d entities / %d relations, want empty", len(g.Entities), len(g.Relations))
	}
}

// TestGraphAsOfTwoAxes drives the bi-temporal matrix: the learned axis
// (transaction time, incl. retirement time-travel), the valid axis, proposal
// exclusion, and project scoping — all derived through the memories join.
func TestGraphAsOfTwoAxes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const user = "graph@x.com"

	t0 := time.Now().Add(-96 * time.Hour).Unix() // learned
	t1 := time.Now().Add(-48 * time.Hour).Unix() // retired
	frag := func(subject, predicate, object string) GraphExtraction {
		return GraphExtraction{
			Entities:  []GraphEntityInput{{Name: subject, Type: "person"}},
			Relations: []GraphRelationInput{{Subject: subject, Predicate: predicate, Object: object}},
		}
	}

	// current: learned at t0, still active.
	current := mustCreateMemory(t, s, "Ada works at Elcano Corp")
	backdateMemory(t, s, current.ID, t0, nil)
	mustReplaceRelations(t, s, current.ID, frag("Ada", "works at", "Elcano Corp"))

	// retired: learned at t0, retired at t1.
	retired := mustCreateMemory(t, s, "Ada works at Initech")
	backdateMemory(t, s, retired.ID, t0, &t1)
	mustReplaceRelations(t, s, retired.ID, frag("Ada", "works at", "Initech"))

	// proposal: never visible, whatever the coordinates.
	prop, err := s.CreateMemoryProposal(ctx, user, "conv-1", MemoryProposalParams{Content: "Ada might use vim"})
	if err != nil {
		t.Fatalf("CreateMemoryProposal: %v", err)
	}
	backdateMemory(t, s, prop.ID, t0, nil)
	mustReplaceRelations(t, s, prop.ID, frag("Ada", "might use", "vim"))

	// windowed: valid only during 2024 (valid time), learned at t0.
	windowed := mustCreateMemory(t, s, "Ada was on-call during 2024")
	vf := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	vt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	if _, err := s.UpdateMemory(ctx, user, windowed.ID, MemoryPatch{ValidFrom: &vf, ValidTo: &vt}); err != nil {
		t.Fatalf("UpdateMemory window: %v", err)
	}
	backdateMemory(t, s, windowed.ID, t0, nil)
	mustReplaceRelations(t, s, windowed.ID, frag("Ada", "was on call", "2024"))

	// project: shared memory scoped to a project, learned at t0.
	proj, err := s.CreateProjectMemory(ctx, "proj-1", user, "team deploys on Fridays", "fact")
	if err != nil {
		t.Fatalf("CreateProjectMemory: %v", err)
	}
	backdateMemory(t, s, proj.ID, t0, nil)
	mustReplaceRelations(t, s, proj.ID, frag("Team", "deploys on", "Fridays"))

	predicates := func(g *Graph) map[string]bool {
		out := map[string]bool{}
		for _, r := range g.Relations {
			out[r.Predicate] = true
		}
		return out
	}

	// Default view (nil = learned-now, no valid filter, personal): the current
	// fact + the windowed fact. Retired, proposed, and project edges excluded.
	g, err := s.GraphAsOf(ctx, user, GraphQuery{})
	if err != nil {
		t.Fatalf("GraphAsOf default: %v", err)
	}
	if got := predicates(g); len(g.Relations) != 2 || !got["works at"] || !got["was on call"] {
		t.Errorf("default view predicates = %v; want {works at, was on call}", got)
	}
	for _, r := range g.Relations {
		if r.Predicate == "works at" && r.ObjectValue != "Elcano Corp" {
			t.Errorf("default view shows %q; the retired Initech edge must be gone", r.ObjectValue)
		}
	}

	// Learned-axis time travel: between t0 and t1 fleet still trusted the
	// Initech fact → BOTH works-at edges are in that historical view.
	mid := time.Unix((t0+t1)/2, 0)
	g, err = s.GraphAsOf(ctx, user, GraphQuery{AsOfLearned: &mid})
	if err != nil {
		t.Fatalf("GraphAsOf mid: %v", err)
	}
	values := map[string]bool{}
	for _, r := range g.Relations {
		if r.Predicate == "works at" {
			values[r.ObjectValue+r.ObjectEntityID] = true
		}
	}
	if len(values) != 2 {
		t.Errorf("as-of-learned(mid) works-at edges = %d, want 2 (retirement is transaction time)", len(values))
	}

	// Before anything was learned: empty graph (and no entities).
	early := time.Unix(t0-3600, 0)
	g, err = s.GraphAsOf(ctx, user, GraphQuery{AsOfLearned: &early})
	if err != nil {
		t.Fatalf("GraphAsOf early: %v", err)
	}
	if len(g.Relations) != 0 || len(g.Entities) != 0 {
		t.Errorf("pre-learning view = %d entities / %d relations, want empty", len(g.Entities), len(g.Relations))
	}

	// Valid-axis: during 2024 the windowed fact was true; in 2026 it is not.
	in2024 := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	g, err = s.GraphAsOf(ctx, user, GraphQuery{AsOfValid: &in2024})
	if err != nil {
		t.Fatalf("GraphAsOf 2024: %v", err)
	}
	if got := predicates(g); !got["was on call"] || !got["works at"] {
		t.Errorf("valid-2024 predicates = %v; want the windowed AND open-ended facts", got)
	}
	in2026 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	g, err = s.GraphAsOf(ctx, user, GraphQuery{AsOfValid: &in2026})
	if err != nil {
		t.Fatalf("GraphAsOf 2026: %v", err)
	}
	if got := predicates(g); got["was on call"] || !got["works at"] {
		t.Errorf("valid-2026 predicates = %v; the expired window must be filtered", got)
	}

	// Project scoping: the project view shows ONLY the project fragment.
	pid := "proj-1"
	g, err = s.GraphAsOf(ctx, user, GraphQuery{ProjectID: &pid})
	if err != nil {
		t.Fatalf("GraphAsOf project: %v", err)
	}
	if len(g.Relations) != 1 || g.Relations[0].Predicate != "deploys on" {
		t.Errorf("project view = %+v; want only the project edge", g.Relations)
	}

	// Cross-user isolation.
	g, err = s.GraphAsOf(ctx, "other@x.com", GraphQuery{})
	if err != nil {
		t.Fatalf("GraphAsOf other user: %v", err)
	}
	if len(g.Relations) != 0 {
		t.Errorf("another user sees %d relations, want 0", len(g.Relations))
	}
}

// TestListMemoriesAsOf pins the flat-record twin: identical two-axis
// semantics over Memory rows.
func TestListMemoriesAsOf(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const user = "graph@x.com"

	t0 := time.Now().Add(-96 * time.Hour).Unix()
	t1 := time.Now().Add(-48 * time.Hour).Unix()

	current := mustCreateMemory(t, s, "works at Elcano Corp")
	backdateMemory(t, s, current.ID, t0, nil)
	retired := mustCreateMemory(t, s, "works at Initech")
	backdateMemory(t, s, retired.ID, t0, &t1)
	if _, err := s.CreateMemoryProposal(ctx, user, "conv-1", MemoryProposalParams{Content: "maybe uses vim"}); err != nil {
		t.Fatalf("CreateMemoryProposal: %v", err)
	}
	windowed := mustCreateMemory(t, s, "on-call during 2024")
	vf := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	vt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	if _, err := s.UpdateMemory(ctx, user, windowed.ID, MemoryPatch{ValidFrom: &vf, ValidTo: &vt}); err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}
	backdateMemory(t, s, windowed.ID, t0, nil)

	ids := func(ms []Memory) map[string]bool {
		out := map[string]bool{}
		for _, m := range ms {
			out[m.ID] = true
		}
		return out
	}

	// Default: current + windowed; retired and proposed excluded.
	ms, err := s.ListMemoriesAsOf(ctx, user, GraphQuery{})
	if err != nil {
		t.Fatalf("ListMemoriesAsOf: %v", err)
	}
	if got := ids(ms); len(ms) != 2 || !got[current.ID] || !got[windowed.ID] {
		t.Errorf("default as-of list = %v", got)
	}

	// Learned-axis time travel resurrects the retired record.
	mid := time.Unix((t0+t1)/2, 0)
	ms, err = s.ListMemoriesAsOf(ctx, user, GraphQuery{AsOfLearned: &mid})
	if err != nil {
		t.Fatalf("ListMemoriesAsOf mid: %v", err)
	}
	if got := ids(ms); !got[retired.ID] || !got[current.ID] {
		t.Errorf("as-of-learned(mid) = %v; want the then-trusted set", got)
	}

	// Valid axis filters the expired window.
	in2026 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	ms, err = s.ListMemoriesAsOf(ctx, user, GraphQuery{AsOfValid: &in2026})
	if err != nil {
		t.Fatalf("ListMemoriesAsOf 2026: %v", err)
	}
	if got := ids(ms); got[windowed.ID] || !got[current.ID] {
		t.Errorf("valid-2026 list = %v; the expired window must be filtered", got)
	}
}

func TestGetMemory(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	m := mustCreateMemory(t, s, "a fact")
	got, err := s.GetMemory(ctx, "graph@x.com", m.ID)
	if err != nil || got.ID != m.ID || got.Content != "a fact" {
		t.Fatalf("GetMemory = %+v, %v", got, err)
	}
	if _, err := s.GetMemory(ctx, "other@x.com", m.ID); err == nil {
		t.Error("GetMemory must be user-scoped")
	}
	if _, err := s.GetMemory(ctx, "graph@x.com", "nope"); err == nil {
		t.Error("GetMemory unknown id should error")
	}
}
