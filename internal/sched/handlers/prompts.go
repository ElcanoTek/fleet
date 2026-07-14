package handlers

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// SetPromptCatalogProvider wires the bundle's live-read prompts/ directory into
// the shared library. nil is a valid empty Git catalog.
func (h *Handlers) SetPromptCatalogProvider(p func() ([]clientconfig.Prompt, []string)) {
	h.promptCatalog = p
}

type PromptLibraryItem struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Description   string    `json:"description,omitempty"`
	Content       string    `json:"content"`
	Source        string    `json:"source"`
	Visibility    string    `json:"visibility"`
	ReadOnly      bool      `json:"read_only"`
	OwnedByCaller bool      `json:"owned_by_caller"`
	OwnerUsername string    `json:"owner_username,omitempty"`
	Path          string    `json:"path,omitempty"`
	CreatedAt     time.Time `json:"created_at,omitempty"`
	UpdatedAt     time.Time `json:"updated_at,omitempty"`
}

type PromptLibraryWrite struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Content     string `json:"content"`
	Visibility  string `json:"visibility"`
}

func promptLibraryItemFromModel(p *models.PromptLibraryEntry, owner string, admin bool) PromptLibraryItem {
	return PromptLibraryItem{
		ID: p.ID.String(), Name: p.Name, Description: p.Description,
		Content: p.Content, Source: "workspace", Visibility: p.Visibility,
		OwnedByCaller: p.OwnerUsername == owner || admin,
		OwnerUsername: p.OwnerUsername, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}

func (p principal) promptIdentity() string {
	if p.user != nil {
		return strings.TrimSpace(p.user.Username)
	}
	if p.apiKey != nil {
		return "apikey:" + p.apiKey.KeyID
	}
	if p.isAdmin {
		return "admin"
	}
	return ""
}

func (h *Handlers) promptItems(r *http.Request) ([]PromptLibraryItem, error) {
	p := h.principalFromRequest(r)
	owner := p.promptIdentity()
	items := make([]PromptLibraryItem, 0)
	if h.promptCatalog != nil {
		gitPrompts, problems := h.promptCatalog()
		for _, problem := range problems {
			log.Printf("prompt library: %s", problem)
		}
		for _, gp := range gitPrompts {
			items = append(items, PromptLibraryItem{
				ID: gp.ID, Name: gp.Name, Description: gp.Description,
				Content: gp.Content, Source: "git", Visibility: "workspace",
				ReadOnly: true, Path: gp.Path,
			})
		}
	}
	dbPrompts, err := h.storage.ListPromptLibrary(r.Context(), owner)
	if err != nil {
		return nil, err
	}
	for _, dp := range dbPrompts {
		items = append(items, promptLibraryItemFromModel(&dp, owner, p.hasPermission(models.PermissionAdmin)))
	}
	return items, nil
}

// PromptLibraryCollection handles GET/POST /prompts.
func (h *Handlers) PromptLibraryCollection(w http.ResponseWriter, r *http.Request) {
	p := h.principalFromRequest(r)
	if !p.hasPermission(models.PermissionViewTasks) {
		writeError(w, http.StatusForbidden, "view permission required")
		return
	}
	if r.Method == http.MethodGet {
		items, err := h.promptItems(r)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to list prompts")
			return
		}
		writeJSON(w, http.StatusOK, items)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !p.hasPermission(models.PermissionCreateTask) {
		writeError(w, http.StatusForbidden, "create permission required")
		return
	}
	var req PromptLibraryWrite
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	entry, err := h.storage.CreatePromptLibrary(r.Context(), p.promptIdentity(), req.Name, req.Description, req.Content, req.Visibility)
	if err != nil {
		switch {
		case errors.Is(err, storage.ErrPromptInvalid):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, storage.ErrPromptConflict):
			writeError(w, http.StatusConflict, "a prompt with that name already exists")
		default:
			writeError(w, http.StatusInternalServerError, "failed to create prompt")
		}
		return
	}
	writeJSON(w, http.StatusCreated, promptLibraryItemFromModel(entry, p.promptIdentity(), p.hasPermission(models.PermissionAdmin)))
}

// PromptLibraryItem handles PUT/DELETE /prompts/{prompt_id}. Git IDs are never
// routed here by the UI and cannot parse as UUIDs, preserving read-only config.
func (h *Handlers) PromptLibraryItem(w http.ResponseWriter, r *http.Request) {
	p := h.principalFromRequest(r)
	if !p.hasPermission(models.PermissionCreateTask) {
		writeError(w, http.StatusForbidden, "create permission required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "prompt_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid prompt id")
		return
	}
	owner := p.promptIdentity()
	admin := p.hasPermission(models.PermissionAdmin)
	switch r.Method {
	case http.MethodPut:
		var req PromptLibraryWrite
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		entry, err := h.storage.UpdatePromptLibrary(r.Context(), id, owner, req.Name, req.Description, req.Content, req.Visibility, admin)
		if err != nil {
			switch {
			case errors.Is(err, storage.ErrPromptInvalid):
				writeError(w, http.StatusBadRequest, err.Error())
			case errors.Is(err, storage.ErrPromptConflict):
				writeError(w, http.StatusConflict, err.Error())
			case errors.Is(err, storage.ErrPromptNotFound):
				writeError(w, http.StatusNotFound, err.Error())
			default:
				writeError(w, http.StatusInternalServerError, "failed to update prompt")
			}
			return
		}
		writeJSON(w, http.StatusOK, promptLibraryItemFromModel(entry, owner, admin))
	case http.MethodDelete:
		if err := h.storage.DeletePromptLibrary(r.Context(), id, owner, admin); err != nil {
			if errors.Is(err, storage.ErrPromptNotFound) {
				writeError(w, http.StatusNotFound, "prompt not found")
			} else {
				writeError(w, http.StatusInternalServerError, "failed to delete prompt")
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (h *Handlers) ExportPromptLibrary(w http.ResponseWriter, r *http.Request) {
	if !h.principalFromRequest(r).hasPermission(models.PermissionViewTasks) {
		writeError(w, http.StatusForbidden, "view permission required")
		return
	}
	items, err := h.promptItems(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to export prompts")
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="fleet-prompts.json"`)
	writeJSON(w, http.StatusOK, map[string]any{
		"format": "fleet-prompt-library", "version": 1,
		"exported_at": time.Now().UTC(), "prompts": items,
	})
}
