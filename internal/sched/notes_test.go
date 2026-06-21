package sched

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/db"
)

func newNotesStore(t *testing.T) *Store {
	t.Helper()
	database := db.New()
	if err := database.Init(""); err != nil {
		t.Skipf("Skipping notes test because DB init failed: %v", err)
	}

	ctx := context.Background()
	conn, err := database.Conn().Conn(ctx)
	if err != nil {
		t.Fatalf("Failed to get DB connection for lock: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock(1)"); err != nil {
		conn.Close()
		t.Fatalf("Failed to acquire test lock: %v", err)
	}
	cleanup := func() {
		database.Conn().ExecContext(ctx, "DELETE FROM agent_note_proposals")
		database.Conn().ExecContext(ctx, "DELETE FROM agent_notes")
	}
	cleanup()
	t.Cleanup(func() {
		cleanup()
		conn.ExecContext(ctx, "SELECT pg_advisory_unlock(1)")
		conn.Close()
		database.Close()
	})
	return NewStore(database)
}

func TestNoteCRUD(t *testing.T) {
	s := newNotesStore(t)
	ctx := context.Background()

	n, err := s.CreateNote(ctx, "xandr-limits", "Xandr Limits", "Max 5/min", "admin@x.com")
	if err != nil {
		t.Fatalf("CreateNote: %v", err)
	}
	if n.Status != "published" || n.Version != 1 {
		t.Fatalf("expected published v1, got status=%s v=%d", n.Status, n.Version)
	}

	// Slug conflict on duplicate create.
	if _, err := s.CreateNote(ctx, "xandr-limits", "dup", "x", "admin@x.com"); !errors.Is(err, ErrSlugConflict) {
		t.Fatalf("expected ErrSlugConflict, got %v", err)
	}

	// Get by id + slug.
	byID, err := s.GetNote(ctx, n.ID)
	if err != nil || byID.Slug != "xandr-limits" {
		t.Fatalf("GetNote: %v / %+v", err, byID)
	}
	bySlug, err := s.GetNoteBySlug(ctx, "xandr-limits")
	if err != nil || bySlug.ID != n.ID {
		t.Fatalf("GetNoteBySlug: %v / %+v", err, bySlug)
	}

	// Update bumps version.
	newBody := "Max 10/min"
	upd, err := s.UpdateNote(ctx, n.ID, nil, &newBody, "admin2@x.com")
	if err != nil {
		t.Fatalf("UpdateNote: %v", err)
	}
	if upd.Version != 2 || upd.Body != "Max 10/min" || upd.UpdatedBy != "admin2@x.com" {
		t.Fatalf("update wrong: v=%d body=%q by=%q", upd.Version, upd.Body, upd.UpdatedBy)
	}

	// Archive (soft-delete) excludes from published list.
	if err := s.ArchiveNote(ctx, n.ID, "admin@x.com"); err != nil {
		t.Fatalf("ArchiveNote: %v", err)
	}
	pub, err := s.ListPublishedNotes(ctx)
	if err != nil {
		t.Fatalf("ListPublishedNotes: %v", err)
	}
	if len(pub) != 0 {
		t.Fatalf("archived note must not appear in published list, got %d", len(pub))
	}
	all, err := s.ListNotes(ctx, true)
	if err != nil {
		t.Fatalf("ListNotes(all): %v", err)
	}
	if len(all) != 1 || all[0].Status != "archived" {
		t.Fatalf("expected 1 archived note in full list, got %+v", all)
	}

	// Not-found paths.
	if _, err := s.GetNoteBySlug(ctx, "nope"); !errors.Is(err, ErrNoteNotFound) {
		t.Fatalf("expected ErrNoteNotFound, got %v", err)
	}
}

func TestNoteSlugValidation(t *testing.T) {
	s := newNotesStore(t)
	ctx := context.Background()
	for _, bad := range []string{"", "Has Space", "UPPER", "with/slash", strings.Repeat("a", 129)} {
		if _, err := s.CreateNote(ctx, bad, "t", "b", "a"); !errors.Is(err, ErrInvalidSlug) {
			t.Errorf("slug %q: expected ErrInvalidSlug, got %v", bad, err)
		}
	}
	for _, ok := range []string{"a", "xandr-limits", "snake_case", "abc123", strings.Repeat("z", 128)} {
		if _, err := s.CreateNote(ctx, ok, "t", "b", "a"); err != nil {
			t.Errorf("slug %q: expected valid, got %v", ok, err)
		}
	}
}

func TestNoteBodyValidation(t *testing.T) {
	s := newNotesStore(t)
	ctx := context.Background()
	huge := strings.Repeat("x", (1<<20)+1)
	if _, err := s.CreateNote(ctx, "huge", "t", huge, "a"); !errors.Is(err, ErrInvalidBody) {
		t.Errorf("expected ErrInvalidBody for >1MiB body, got %v", err)
	}
}

func TestProposalLifecycle_PublishCreateNew(t *testing.T) {
	s := newNotesStore(t)
	ctx := context.Background()

	// Propose a brand-new note (slug not yet in agent_notes → note_id NULL).
	p, err := s.CreateProposal(ctx, "new-playbook", "New Playbook", "step 1", "found a pattern", "scheduled-task-42")
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}
	if p.Status != "pending" || p.NoteID != nil {
		t.Fatalf("expected pending create-new proposal, got status=%s noteID=%v", p.Status, p.NoteID)
	}

	pending, err := s.ListProposals(ctx, "pending")
	if err != nil || len(pending) != 1 {
		t.Fatalf("ListProposals(pending): %v len=%d", err, len(pending))
	}

	// Publish materializes a new note (version 1) and marks the proposal published.
	note, err := s.PublishProposal(ctx, p.ID, "admin@x.com", "looks good")
	if err != nil {
		t.Fatalf("PublishProposal: %v", err)
	}
	if note.Slug != "new-playbook" || note.Version != 1 || note.Body != "step 1" {
		t.Fatalf("published note wrong: %+v", note)
	}
	decided, err := s.GetProposal(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetProposal: %v", err)
	}
	if decided.Status != "published" || decided.DecidedBy != "admin@x.com" || decided.DecisionNote != "looks good" {
		t.Fatalf("proposal not marked published: %+v", decided)
	}

	// Publishing again must fail (no longer pending).
	if _, err := s.PublishProposal(ctx, p.ID, "admin@x.com", ""); err == nil {
		t.Fatal("expected re-publish of a decided proposal to fail")
	}
}

func TestProposalLifecycle_PublishUpdateExisting(t *testing.T) {
	s := newNotesStore(t)
	ctx := context.Background()

	base, err := s.CreateNote(ctx, "rate-caps", "Rate Caps", "old body", "admin@x.com")
	if err != nil {
		t.Fatalf("CreateNote: %v", err)
	}

	// A proposal targeting the existing slug carries the resolved note_id.
	p, err := s.CreateProposal(ctx, "rate-caps", "Rate Caps", "new body", "rate changed", "chat-user@x.com")
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}
	if p.NoteID == nil || *p.NoteID != base.ID {
		t.Fatalf("expected proposal note_id = %s, got %v", base.ID, p.NoteID)
	}

	// Publishing updates the existing note and bumps version (1 → 2).
	updated, err := s.PublishProposal(ctx, p.ID, "admin@x.com", "")
	if err != nil {
		t.Fatalf("PublishProposal: %v", err)
	}
	if updated.ID != base.ID {
		t.Fatalf("expected same note id, got %s vs %s", updated.ID, base.ID)
	}
	if updated.Version != 2 || updated.Body != "new body" {
		t.Fatalf("expected v2 with new body, got v=%d body=%q", updated.Version, updated.Body)
	}
}

func TestProposalLifecycle_Reject(t *testing.T) {
	s := newNotesStore(t)
	ctx := context.Background()

	p, err := s.CreateProposal(ctx, "bad-idea", "Bad Idea", "nope", "agent reasoning", "scheduled-task-1")
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}
	if err := s.RejectProposal(ctx, p.ID, "admin@x.com", "contains a secret"); err != nil {
		t.Fatalf("RejectProposal: %v", err)
	}
	decided, _ := s.GetProposal(ctx, p.ID)
	if decided.Status != "rejected" || decided.DecisionNote != "contains a secret" {
		t.Fatalf("expected rejected with reason, got %+v", decided)
	}
	// agent_notes is untouched (no note created).
	if _, err := s.GetNoteBySlug(ctx, "bad-idea"); !errors.Is(err, ErrNoteNotFound) {
		t.Fatalf("reject must not create a note, got %v", err)
	}
	// Re-rejecting a decided proposal does nothing (no longer pending).
	if err := s.RejectProposal(ctx, p.ID, "admin@x.com", "again"); !errors.Is(err, ErrNoteNotFound) {
		t.Fatalf("expected re-reject to report not-pending/not-found, got %v", err)
	}
}

// notesProviderAdapter shows the Store satisfies the agentcore NotesProvider
// shape (Slug/Title/Body) via ListPublishedNotes — exercised here at the data
// layer; the actual seam wiring happens in the process (P6).
func TestListPublishedNotesOrdering(t *testing.T) {
	s := newNotesStore(t)
	ctx := context.Background()

	if _, err := s.CreateNote(ctx, "first", "First", "a", "admin"); err != nil {
		t.Fatalf("CreateNote first: %v", err)
	}
	if _, err := s.CreateNote(ctx, "second", "Second", "b", "admin"); err != nil {
		t.Fatalf("CreateNote second: %v", err)
	}
	// Touch "first" so it has the newest updated_at.
	f, _ := s.GetNoteBySlug(ctx, "first")
	nb := "a2"
	if _, err := s.UpdateNote(ctx, f.ID, nil, &nb, "admin"); err != nil {
		t.Fatalf("UpdateNote: %v", err)
	}
	notes, err := s.ListPublishedNotes(ctx)
	if err != nil {
		t.Fatalf("ListPublishedNotes: %v", err)
	}
	if len(notes) != 2 {
		t.Fatalf("expected 2 published notes, got %d", len(notes))
	}
	if notes[0].Slug != "first" {
		t.Errorf("expected most-recently-updated note first, got %q", notes[0].Slug)
	}
}
