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

// TestLabels_ArrayRoundTrip pins the text[] encoding contract that replaced
// lib/pq's pq.Array (which was dropped along with the unmaintained driver — see
// internal/sched/db/migrate.go). Binding relies on the pgx stdlib driver
// encoding a plain []string, and reading relies on textArray decoding the
// text-format array literal database/sql hands back. Labels are user-supplied,
// so the values that would break a naive literal parser are the interesting
// ones: separators, quotes, backslashes, braces, and the bare word NULL.
func TestLabels_ArrayRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	labels := []string{`a,b`, `he said "hi"`, `back\slash`, `{braces}`, `NULL`, `héllo→`}
	id := seedConv(t, s, "round-trip", labels)

	c, err := s.Get(ctx, labelTestUser, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if c == nil {
		t.Fatal("Get returned no conversation")
	}
	if len(c.Labels) != len(labels) {
		t.Fatalf("labels = %#v, want %#v", c.Labels, labels)
	}
	for i := range labels {
		if c.Labels[i] != labels[i] {
			t.Errorf("label %d = %q, want %q", i, c.Labels[i], labels[i])
		}
	}

	// The same values must survive the filter path, which binds []string into
	// `labels @> $3::text[]` rather than scanning it back out.
	got, err := s.ListFiltered(ctx, labelTestUser, ListFilter{Labels: []string{`a,b`, `{braces}`}})
	if err != nil {
		t.Fatalf("ListFiltered: %v", err)
	}
	if len(got) != 1 || got[0].ID != id {
		t.Fatalf("ListFiltered matched %d rows, want just the seeded one", len(got))
	}
}

// TestLabels_EmptyAndUnset covers the two absent-value shapes textArray must
// distinguish from a populated array: a conversation created without labels and
// one explicitly patched to none.
func TestLabels_EmptyAndUnset(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	unset := seedConv(t, s, "unset", nil)
	c, err := s.Get(ctx, labelTestUser, unset)
	if err != nil {
		t.Fatalf("Get(unset): %v", err)
	}
	if len(c.Labels) != 0 {
		t.Errorf("unset labels = %#v, want empty", c.Labels)
	}

	cleared := seedConv(t, s, "cleared", []string{"tmp"})
	if _, err := s.BulkPatch(ctx, labelTestUser, []string{cleared}, nil, []string{}); err != nil {
		t.Fatalf("BulkPatch(clear): %v", err)
	}
	c, err = s.Get(ctx, labelTestUser, cleared)
	if err != nil {
		t.Fatalf("Get(cleared): %v", err)
	}
	if len(c.Labels) != 0 {
		t.Errorf("cleared labels = %#v, want empty", c.Labels)
	}
}
