package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched"
)

// Notes REST handlers (NOTES_WIKI_SPEC §6): admin-curated knowledge-base CRUD +
// agent-proposal curation. Admin-gating is applied by the route groups in
// cmd/fleet (CRUD + decisions under AdminAuthMiddleware; reads under
// AdminOrUserAuthMiddleware), mirroring how /stats and /tasks are gated.
//
// HTTP codes: 200/201/204 success · 400 validation · 404 not found · 409 slug
// conflict · 500 operational.
type NotesHandlers struct {
	store *sched.Store
	h     *Handlers // shared helpers (CSRF + response helpers live on the package)
}

// NewNotesHandlers builds the notes handler set over the sched notes store.
func NewNotesHandlers(store *sched.Store, h *Handlers) *NotesHandlers {
	return &NotesHandlers{store: store, h: h}
}

// noteUpsert is the create/update body.
type noteUpsert struct {
	Slug  string `json:"slug"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

// ListNotes — GET /notes?all=1
func (n *NotesHandlers) ListNotes(w http.ResponseWriter, r *http.Request) {
	includeArchived := r.URL.Query().Get("all") == "1"
	notes, err := n.store.ListNotes(r.Context(), includeArchived)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"notes": notes})
}

// GetNote — GET /notes/{slug}
func (n *NotesHandlers) GetNote(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	note, err := n.store.GetNoteBySlug(r.Context(), slug)
	if errors.Is(err, sched.ErrNoteNotFound) {
		writeError(w, http.StatusNotFound, "note not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, note)
}

// CreateNote — POST /notes
func (n *NotesHandlers) CreateNote(w http.ResponseWriter, r *http.Request) {
	var body noteUpsert
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	note, err := n.store.CreateNote(r.Context(), body.Slug, body.Title, body.Body, actorEmail(r))
	switch {
	case errors.Is(err, sched.ErrSlugConflict):
		writeError(w, http.StatusConflict, "note slug already exists")
		return
	case errors.Is(err, sched.ErrInvalidSlug), errors.Is(err, sched.ErrInvalidBody):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, note)
}

// UpdateNote — PUT /notes/{slug}
func (n *NotesHandlers) UpdateNote(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	existing, err := n.store.GetNoteBySlug(r.Context(), slug)
	if errors.Is(err, sched.ErrNoteNotFound) {
		writeError(w, http.StatusNotFound, "note not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var body noteUpsert
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	title, bodyText := body.Title, body.Body
	note, err := n.store.UpdateNote(r.Context(), existing.ID, &title, &bodyText, actorEmail(r))
	switch {
	case errors.Is(err, sched.ErrNoteNotFound):
		writeError(w, http.StatusNotFound, "note not found")
		return
	case errors.Is(err, sched.ErrInvalidBody):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, note)
}

// ArchiveNote — DELETE /notes/{slug}
func (n *NotesHandlers) ArchiveNote(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	existing, err := n.store.GetNoteBySlug(r.Context(), slug)
	if errors.Is(err, sched.ErrNoteNotFound) {
		writeError(w, http.StatusNotFound, "note not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := n.store.ArchiveNote(r.Context(), existing.ID, actorEmail(r)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListProposals — GET /notes/proposals?status=pending
func (n *NotesHandlers) ListProposals(w http.ResponseWriter, r *http.Request) {
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	props, err := n.store.ListProposals(r.Context(), status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"proposals": props})
}

// GetProposal — GET /notes/proposals/{id}
func (n *NotesHandlers) GetProposal(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUID(w, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	prop, err := n.store.GetProposal(r.Context(), id)
	if errors.Is(err, sched.ErrNoteNotFound) {
		writeError(w, http.StatusNotFound, "proposal not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Best-effort current note for the diff view.
	current, _ := n.store.GetNoteBySlug(r.Context(), prop.Slug)
	writeJSON(w, http.StatusOK, map[string]any{"proposal": prop, "current": current})
}

// PublishProposal — POST /notes/proposals/{id}/publish
func (n *NotesHandlers) PublishProposal(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUID(w, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	var body struct {
		Note string `json:"note"`
	}
	if r.ContentLength > 0 {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	note, err := n.store.PublishProposal(r.Context(), id, actorEmail(r), body.Note)
	if errors.Is(err, sched.ErrNoteNotFound) {
		writeError(w, http.StatusNotFound, "proposal not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, note)
}

// RejectProposal — POST /notes/proposals/{id}/reject
func (n *NotesHandlers) RejectProposal(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUID(w, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength > 0 {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	if strings.TrimSpace(body.Reason) == "" {
		writeError(w, http.StatusBadRequest, "reason is required")
		return
	}
	if err := n.store.RejectProposal(r.Context(), id, actorEmail(r), body.Reason); err != nil {
		if errors.Is(err, sched.ErrNoteNotFound) {
			writeError(w, http.StatusNotFound, "pending proposal not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	prop, _ := n.store.GetProposal(r.Context(), id)
	writeJSON(w, http.StatusOK, prop)
}

// parseUUID parses a path UUID param, writing a 400 on failure.
func parseUUID(w http.ResponseWriter, raw string) (uuid.UUID, bool) {
	id, err := uuid.Parse(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return uuid.Nil, false
	}
	return id, true
}

// actorEmail returns a best-effort actor identity for created_by / decided_by.
// The auth middleware sets a principal; absent one (admin-key path), "admin".
func actorEmail(r *http.Request) string {
	if v := r.Header.Get("X-User-Email"); v != "" {
		return strings.ToLower(strings.TrimSpace(v))
	}
	return "admin"
}
