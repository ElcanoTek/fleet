package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// SetPromptCatalogProvider wires the bundle's live-read prompts/ directory into
// the shared library. nil is a valid empty Git catalog.
func (h *Handlers) SetPromptCatalogProvider(p func() ([]clientconfig.Prompt, []string)) {
	h.promptCatalog = p
}

// ChatMemberRoleProvider resolves a chat-plane member's role ("admin",
// "member", or "viewer") by email, returning ("", nil) when the email is not a
// provisioned chat user. Because the Prompt Library is shared between Chat
// (composer picker, "Save as workflow…", and Settings → Prompts) and the
// Operations Center, header-trust requests to /prompts fall back to this seam
// when the caller's independent Operations Center role (ops_role) is "none".
type ChatMemberRoleProvider func(ctx context.Context, email string) (string, error)

// SetChatMemberRoleProvider wires the chat-plane role lookup used to authorize
// signed-in Chat users on /prompts when they have no sched.users row.
func (h *Handlers) SetChatMemberRoleProvider(fn ChatMemberRoleProvider) {
	h.chatMemberRole = fn
}

func isPromptLibraryPath(path string) bool {
	return path == "/prompts" || strings.HasPrefix(path, "/prompts/")
}

func (h *Handlers) lookupChatMemberForPrompts(ctx context.Context, email string) (*models.User, error) {
	if h.chatMemberRole == nil {
		return nil, nil
	}
	role, err := h.chatMemberRole(ctx, email)
	if err != nil {
		return nil, err
	}
	var mapped string
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "admin":
		mapped = "admin"
	case "member":
		mapped = "client"
	case "viewer":
		mapped = "readonly"
	default:
		return nil, nil
	}
	return &models.User{
		Username: strings.ToLower(strings.TrimSpace(email)),
		Role:     mapped,
	}, nil
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
	// Fields and PromptTemplate make a Git entry a form prompt: the picker
	// collects the fields and inserts the rendered template instead of
	// Content. Only Git entries that declare a valid form carry them (see
	// clientconfig.parsePromptForm); workspace (database) prompts never do.
	Fields         []clientconfig.PromptField `json:"fields,omitempty"`
	PromptTemplate string                     `json:"prompt_template,omitempty"`
}

type PromptLibraryWrite struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Content     string `json:"content"`
	Visibility  string `json:"visibility"`
}

// promptLibraryItemFromGit is the wire item for one bundle prompt. Git entries
// are workspace-visible and read-only to everyone, admins included: the file in
// the config repository is the source of truth.
func promptLibraryItemFromGit(gp clientconfig.Prompt) PromptLibraryItem {
	return PromptLibraryItem{
		ID: gp.ID, Name: gp.Name, Description: gp.Description,
		Content: gp.Content, Source: "git", Visibility: "workspace",
		ReadOnly: true, Path: gp.Path,
		Fields: gp.Fields, PromptTemplate: gp.PromptTemplate,
	}
}

func promptLibraryItemFromModel(p *models.PromptLibraryEntry, owner string, admin bool) PromptLibraryItem {
	item := PromptLibraryItem{
		ID: p.ID.String(), Name: p.Name, Description: p.Description,
		Content: p.Content, Source: "workspace", Visibility: p.Visibility,
		OwnedByCaller: p.OwnerUsername == owner || admin,
		OwnerUsername: p.OwnerUsername, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
	if fields, tmpl, issues := clientconfig.ParsePromptForm([]byte(p.Content)); len(issues) == 0 && len(fields) > 0 {
		item.Fields = fields
		item.PromptTemplate = tmpl
	}
	return item
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
			logging.From(r.Context()).Warn("prompt library: problem", "problem", problem)
		}
		for _, gp := range gitPrompts {
			items = append(items, promptLibraryItemFromGit(gp))
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
