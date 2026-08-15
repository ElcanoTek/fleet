package store

import (
	"context"
	"testing"
)

// labelTestUser is the single owner used across the label store tests.
const labelTestUser = "u@x.com"

// seedConv creates a conversation owned by labelTestUser and (optionally)
// assigns labels via BulkPatch, returning its id. Nil labels leaves the default.
func seedConv(t *testing.T, s *Store, title string, labels []string) string {
	t.Helper()
	ctx := context.Background()
	c, err := s.CreateConversation(ctx, labelTestUser, title, "victoria", "m", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if labels != nil {
		if _, err := s.BulkPatch(ctx, labelTestUser, []string{c.ID}, nil, labels); err != nil {
			t.Fatalf("BulkPatch: %v", err)
		}
	}
	return c.ID
}

// TestListFiltered_Labels covers the #258 filter semantics: single-label match,
// AND across multiple labels, and the unfiltered baseline.
func TestListFiltered_Labels(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const u = labelTestUser

	work1 := seedConv(t, s, "w1", []string{"go", "urgent"})
	seedConv(t, s, "w2", []string{"go"})
	seedConv(t, s, "r1", []string{"urgent"})
	seedConv(t, s, "n1", nil)

	if got, err := s.ListFiltered(ctx, u, ListFilter{Labels: []string{"go"}}); err != nil || len(got) != 2 {
		t.Fatalf("label=go: got %d (err %v), want 2", len(got), err)
	}
	// AND semantics: only work1 has both labels.
	got, err := s.ListFiltered(ctx, u, ListFilter{Labels: []string{"go", "urgent"}})
	if err != nil || len(got) != 1 || got[0].ID != work1 {
		t.Fatalf("label=go&urgent: got %d (err %v), want 1 (work1)", len(got), err)
	}
	// No filter → all four.
	if got, err := s.ListFiltered(ctx, u, ListFilter{}); err != nil || len(got) != 4 {
		t.Fatalf("no filter: got %d (err %v), want 4", len(got), err)
	}
}
