package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ElcanoTek/fleet/internal/sched"
	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// setupNotesTest wires the notes routes over the shared sched test DB. Auth
// middleware is applied in cmd/fleet; here we exercise the handlers directly so
// the test asserts status codes + state transitions, not the auth gate. The
// returned *sched.Store is the SAME store the routes use, for staging proposals.
func setupNotesTest(t *testing.T) (*chi.Mux, *sched.Store, func()) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "notes-test-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	store := storage.New()
	if err := store.Initialize(filepath.Join(tmpDir, "test.db")); err != nil {
		os.RemoveAll(tmpDir)
		if isDatabaseUnavailable(err) {
			t.Skipf("database unavailable: %v", err)
		}
		t.Fatalf("init storage: %v", err)
	}
	acquireTestLock(t, store)

	ctx := context.Background()
	for _, q := range []string{"DELETE FROM agent_note_proposals", "DELETE FROM agent_notes"} {
		if _, err := store.DB().Conn().ExecContext(ctx, q); err != nil {
			t.Fatalf("clean notes tables: %v", err)
		}
	}

	keyMgr, _ := apikeys.NewManager(filepath.Join(tmpDir, "keys.json"), filepath.Join(tmpDir, "audit.jsonl"))
	h := New(Config{Version: "test"}, store, keyMgr)
	notesStore := sched.NewStore(store.DB())
	nh := NewNotesHandlers(notesStore, h)

	r := chi.NewRouter()
	r.Post("/notes", nh.CreateNote)
	r.Get("/notes", nh.ListNotes)
	r.Get("/notes/{slug}", nh.GetNote)
	r.Put("/notes/{slug}", nh.UpdateNote)
	r.Delete("/notes/{slug}", nh.ArchiveNote)
	r.Get("/notes/proposals", nh.ListProposals)
	r.Get("/notes/proposals/{id}", nh.GetProposal)
	r.Post("/notes/proposals/{id}/publish", nh.PublishProposal)
	r.Post("/notes/proposals/{id}/reject", nh.RejectProposal)

	cleanup := func() {
		store.Close()
		os.RemoveAll(tmpDir)
	}
	return r, notesStore, cleanup
}

func doJSON(t *testing.T, r http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestNotesHandlers_CRUD(t *testing.T) {
	r, _, cleanup := setupNotesTest(t)
	defer cleanup()

	// Create.
	w := doJSON(t, r, http.MethodPost, "/notes", map[string]string{"slug": "rate-limits", "title": "Rate Limits", "body": "5/day"})
	if w.Code != http.StatusCreated {
		t.Fatalf("create note: got %d, want 201 (body=%s)", w.Code, w.Body.String())
	}

	// Duplicate slug → 409.
	w = doJSON(t, r, http.MethodPost, "/notes", map[string]string{"slug": "rate-limits", "title": "x", "body": "y"})
	if w.Code != http.StatusConflict {
		t.Errorf("duplicate create: got %d, want 409", w.Code)
	}

	// Invalid slug → 400.
	w = doJSON(t, r, http.MethodPost, "/notes", map[string]string{"slug": "Bad Slug!", "title": "x", "body": "y"})
	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid slug: got %d, want 400", w.Code)
	}

	// Get.
	w = doJSON(t, r, http.MethodGet, "/notes/rate-limits", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get note: got %d, want 200", w.Code)
	}

	// Get missing → 404.
	w = doJSON(t, r, http.MethodGet, "/notes/missing", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("get missing: got %d, want 404", w.Code)
	}

	// Update bumps version.
	w = doJSON(t, r, http.MethodPut, "/notes/rate-limits", map[string]string{"slug": "rate-limits", "title": "Rate Limits", "body": "10/day"})
	if w.Code != http.StatusOK {
		t.Fatalf("update note: got %d, want 200", w.Code)
	}
	var updated sched.Note
	_ = json.Unmarshal(w.Body.Bytes(), &updated)
	if updated.Version != 2 || updated.Body != "10/day" {
		t.Errorf("update result = v%d body=%q, want v2 10/day", updated.Version, updated.Body)
	}

	// Archive (soft-delete) → 204, then excluded from default list.
	w = doJSON(t, r, http.MethodDelete, "/notes/rate-limits", nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("archive note: got %d, want 204", w.Code)
	}
	w = doJSON(t, r, http.MethodGet, "/notes", nil)
	var listResp struct {
		Notes []sched.Note `json:"notes"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &listResp)
	if len(listResp.Notes) != 0 {
		t.Errorf("archived note still listed: %d notes", len(listResp.Notes))
	}
	// ?all=1 includes archived.
	w = doJSON(t, r, http.MethodGet, "/notes?all=1", nil)
	_ = json.Unmarshal(w.Body.Bytes(), &listResp)
	if len(listResp.Notes) != 1 {
		t.Errorf("?all=1 list = %d notes, want 1 (archived)", len(listResp.Notes))
	}
}

func TestNotesHandlers_ProposalPublishReject(t *testing.T) {
	r, store, cleanup := setupNotesTest(t)
	defer cleanup()

	// Stage two proposals directly through the store (agents propose; the REST
	// layer only curates).
	ctx := context.Background()
	p1, err := store.CreateProposal(ctx, "new-playbook", "New Playbook", "step 1", "discovered", "task-7")
	if err != nil {
		t.Fatalf("create proposal 1: %v", err)
	}
	p2, err := store.CreateProposal(ctx, "another-note", "Another", "body", "found", "task-8")
	if err != nil {
		t.Fatalf("create proposal 2: %v", err)
	}

	// List pending.
	w := doJSON(t, r, http.MethodGet, "/notes/proposals?status=pending", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list proposals: got %d, want 200", w.Code)
	}

	// Publish p1 → materializes a note.
	w = doJSON(t, r, http.MethodPost, "/notes/proposals/"+p1.ID.String()+"/publish", map[string]string{"note": "looks good"})
	if w.Code != http.StatusOK {
		t.Fatalf("publish: got %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	w = doJSON(t, r, http.MethodGet, "/notes/new-playbook", nil)
	if w.Code != http.StatusOK {
		t.Errorf("published note not found: got %d", w.Code)
	}

	// Reject p2 with a reason → proposal rejected, no note created.
	w = doJSON(t, r, http.MethodPost, "/notes/proposals/"+p2.ID.String()+"/reject", map[string]string{"reason": "not durable"})
	if w.Code != http.StatusOK {
		t.Fatalf("reject: got %d, want 200", w.Code)
	}
	w = doJSON(t, r, http.MethodGet, "/notes/another-note", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("rejected proposal should not create a note: got %d, want 404", w.Code)
	}

	// Reject without reason → 400.
	p3, _ := store.CreateProposal(ctx, "third", "Third", "b", "", "task-9")
	w = doJSON(t, r, http.MethodPost, "/notes/proposals/"+p3.ID.String()+"/reject", map[string]string{})
	if w.Code != http.StatusBadRequest {
		t.Errorf("reject without reason: got %d, want 400", w.Code)
	}
}
